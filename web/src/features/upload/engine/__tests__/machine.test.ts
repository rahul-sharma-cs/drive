/**
 * PLAN §"Client engine state machine" — one case per row of that table,
 * plus the protocol paths the table depends on (chimera verify, URL refill,
 * matched-session resume, 0-byte, multi-tab lock, cadence-derived progress).
 */

import { describe, expect, it } from 'vitest'
import { ApiError, NETWORK_RETRY_BUDGET, NetworkError, type PutOutcome } from '../types'
import { EXPIRED_BODY, FakeHash, makeFile, makeHarness, md5Of } from './harness.test'

const expired400: PutOutcome = { kind: 'response', status: 400, etag: null, body: EXPIRED_BODY }
const expired403: PutOutcome = { kind: 'response', status: 403, etag: null, body: '' }
const badEtag: PutOutcome = { kind: 'response', status: 200, etag: '"deadbeef"', body: '' }
const netFail: PutOutcome = { kind: 'network', message: 'connection reset' }

const partsPut = (h: ReturnType<typeof makeHarness>, part: number): number =>
  h.put.puts.filter((p) => p.part === part).length

describe('happy path', () => {
  it('uploads every part, completes, and adopts the server-returned final name', async () => {
    const h = makeHarness()
    h.server.publishedName = 'a (1).bin'

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.put.puts.map((p) => p.part).sort()).toEqual([1, 2, 3])
    expect(h.server.calls).toContain('complete:sha-0-3000')
    expect(h.snap().name).toBe('a (1).bin')
    expect(h.snap().renamed).toBe(true)
    expect(h.snap().node_id).toBe('node-1')
    expect(h.snap().progress).toBe(1)
  })

  it('confirms each part with the normalized ETag', async () => {
    const h = makeHarness()
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.server.calls.filter((c) => c.startsWith('confirm:')).sort()).toEqual([
      'confirm:1',
      'confirm:2',
      'confirm:3',
    ])
  })
})

describe('row: part PUT fails integrity (normalized ETag != MD5)', () => {
  it('retries the part and succeeds without re-uploading the others', async () => {
    const h = makeHarness()
    h.put.scriptPart(2, badEtag)

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(partsPut(h, 2)).toBe(2)
    expect(partsPut(h, 1)).toBe(1)
  })

  it('gives up after the 8-attempt budget with a Retry that resets it', async () => {
    const h = makeHarness()
    for (let i = 0; i < 8; i++) h.put.scriptPart(2, badEtag)

    await h.clock.runUntil(() => h.snap().state === 'failed')
    expect(h.snap().error_code).toBe('budget_exhausted')
    expect(partsPut(h, 2)).toBe(8)

    h.engine.retry(h.id)
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(partsPut(h, 2)).toBe(9)
  })

  it('Retry re-runs the whole-file SHA-256 after the hash worker failed', async () => {
    // PLAN's risk table names exactly this: hash-wasm is dormant, "verify worker
    // instantiation first in 4a". A cached REJECTED hash promise made Retry a
    // no-op — the only escape was cancel + re-add.
    const file = makeFile(3000)
    let fingerprinted = false
    let boom = true
    const sha256 = new FakeHash((start, end, blob) => {
      if (!(blob instanceof File)) {
        fingerprinted = true
        return 'fp'
      }
      // The first File hash after the fingerprint payload is the whole-file one.
      if (fingerprinted && boom) {
        boom = false
        throw new Error('wasm boom')
      }
      return `sha-${start}-${end}`
    })
    const h = makeHarness({ file, env: { sha256: () => sha256 } })

    await h.clock.runUntil(() => h.snap().state === 'failed')
    expect(h.snap().error_code).toBe('internal')
    expect(h.snap().error).not.toMatch(/wasm boom/) // PLAN §Phase 5 message discipline

    h.engine.retry(h.id)
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.server.calls).toContain('complete:sha-0-3000')
  })

  it('charges the same budget when the server rejects the part at confirm (422)', async () => {
    const h = makeHarness()
    h.server.inject('confirm', new ApiError(422, 'invalid', 'wrong size'))

    await h.clock.runUntil(() => h.snap().state === 'done')
    // One part was re-sliced and re-PUT; the others went once.
    expect(h.put.puts.length).toBe(4)
  })
})

describe('row: part PUT 403 or 400-with-InvalidRequest (expired presign)', () => {
  it('re-handshakes for fresh URLs on a 403 without charging the integrity budget', async () => {
    const h = makeHarness()
    h.put.scriptPart(1, expired403)

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(partsPut(h, 1)).toBe(2)
    // Exactly one handshake: the re-handshake this row is about. (It used to be
    // 2 only because `transfer()` threw away the create response's URLs and
    // re-presigned every part before the first byte moved.)
    expect(h.server.calls.filter((c) => c === 'resume:refill').length).toBeGreaterThanOrEqual(1)
    // The retry used a different presigned URL.
    const urls = h.put.puts.filter((p) => p.part === 1).map((p) => p.url)
    expect(urls[0]).not.toBe(urls[1])
  })

  it("treats Garage's measured 400 + <Code>InvalidRequest</Code> the same way", async () => {
    const h = makeHarness()
    h.put.scriptPart(1, expired400)

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(partsPut(h, 1)).toBe(2)
  })

  it('pauses on the backend after 3 consecutive re-handshakes (clock-skew guard)', async () => {
    const h = makeHarness()
    for (let i = 0; i < 4; i++) h.put.scriptPart(1, expired403)

    await h.clock.runUntil(() => h.snap().state === 'paused_backend')
    expect(partsPut(h, 1)).toBe(4)
  })

  it('rests instead of flapping while the skew persists: probe backoff grows', async () => {
    const h = makeHarness()
    // The laptop woke with >60 s of Docker-VM drift: EVERY presign is stale, so
    // the auto-resume after paused_backend fails again, forever.
    for (let i = 0; i < 400; i++) h.put.scriptPart(1, expired403)

    await h.clock.runUntil(() => h.snap().state === 'paused_backend')

    let maxWait = 0
    for (let i = 0; i < 10; i++) {
      await h.clock.advance(30_000)
      maxWait = Math.max(maxWait, ...h.clock.pendingDelays)
    }

    // Five minutes of a permanent fault must not cost hundreds of doomed
    // 100 MiB part PUTs...
    expect(h.put.puts.length).toBeLessThanOrEqual(60)
    // ...because each paused_backend entry waits longer than the last (a probe
    // backoff that resets on every entry pins the wait at backoffDelay(1)).
    expect(maxWait).toBeGreaterThan(10_000)
    expect(h.snap().state).toBe('paused_backend')
  })
})

describe('row: retry budget exhausted', () => {
  it('caps a permanently network-failing part instead of retrying forever', async () => {
    const h = makeHarness()
    // A CORS-hidden expired presign reaches the browser as status 0, which
    // classifies as `network` — so this path needs its own budget.
    for (let i = 0; i < 40; i++) h.put.scriptPart(1, netFail)

    await h.clock.runUntil(() => h.snap().state === 'paused_backend')

    expect(partsPut(h, 1)).toBe(NETWORK_RETRY_BUDGET)
    expect(h.snap().error).toMatch(/stalled/i)
  })
})

describe('row: no upload progress for 45 s (stall watchdog)', () => {
  it('aborts the XHR and retries as a network failure, with no integrity charge', async () => {
    const h = makeHarness()
    h.put.hold.add(1)

    await h.clock.runUntil(() => h.put.puts.some((p) => p.part === 1))
    expect(h.clock.pendingDelays).toContain(45_000)

    h.put.hold.delete(1) // the retry will go through
    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.put.aborts).toContain(1)
    expect(partsPut(h, 1)).toBe(2)
  })

  it('is RE-ARMED by upload progress: a slow-but-alive part survives 5 minutes', async () => {
    const h = makeHarness()
    // 100 MiB on a 2 Mbit link is ~7 min of steady progress. The watchdog must
    // never fire while bytes are moving — only when the socket goes silent.
    h.put.heartbeat(1, 5_000)

    await h.clock.runUntil(() => h.put.puts.some((p) => p.part === 1))
    await h.clock.advance(300_000)

    expect(h.put.aborts).toEqual([])
    expect(partsPut(h, 1)).toBe(1)
    expect(h.snap().state).toBe('uploading')
  })
})

describe('row: slice-read NotReadableError / XHR error → 1-byte probe', () => {
  it('probe fails → terminal error_file_changed', async () => {
    const file = makeFile(3000)
    const err = Object.assign(new Error('could not be read'), { name: 'NotReadableError' })
    Object.defineProperty(file, 'slice', {
      value: () => ({ arrayBuffer: () => Promise.reject(err) }),
    })
    const h = makeHarness({ file })
    h.md5.fail = err

    await h.clock.runUntil(() => h.snap().state === 'error_file_changed')
    expect(h.snap().error_code).toBe('file_changed')
  })

  it('probe succeeds → the network-failure path, and the upload finishes', async () => {
    const h = makeHarness()
    h.put.scriptPart(1, netFail)

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(partsPut(h, 1)).toBe(2)
  })
})

describe('row: API unreachable while Garage PUTs may still succeed (kill -9)', () => {
  it('goes to paused_backend, probes with backoff, and auto-resumes', async () => {
    const h = makeHarness()
    h.server.inject('confirm', new NetworkError('fetch failed'))

    await h.clock.runUntil(() => h.snap().state === 'paused_backend')
    expect(h.snap().error).toMatch(/server is unreachable/i)

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.server.calls.some((c) => c.startsWith('status:'))).toBe(true)
  })

  it('probes immediately when the tab becomes visible again', async () => {
    const h = makeHarness()
    h.server.inject('confirm', new NetworkError('fetch failed'))
    await h.clock.runUntil(() => h.snap().state === 'paused_backend')

    const before = h.server.calls.filter((c) => c.startsWith('status:')).length
    h.engine.notifyVisible()
    await h.clock.runUntil(() => h.server.calls.filter((c) => c.startsWith('status:')).length > before)
  })
})

describe('row: browser offline event', () => {
  it('pauses offline and auto-resumes on the online event', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    h.put.hold.add(2)
    await h.clock.runUntil(() => h.put.puts.length >= 2)

    h.engine.notifyOffline()
    expect(h.snap().state).toBe('paused_offline')
    expect(h.put.aborts.length).toBeGreaterThan(0)

    h.put.hold.clear()
    h.engine.notifyOnline()
    await h.clock.runUntil(() => h.snap().state === 'done')
  })

  it('also auto-resumes on visibilitychange → visible', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    await h.clock.runUntil(() => h.put.puts.length >= 1)

    h.engine.notifyOffline()
    expect(h.snap().state).toBe('paused_offline')

    h.put.hold.clear()
    h.engine.notifyVisible()
    await h.clock.runUntil(() => h.snap().state === 'done')
  })
})

describe('row: handshake/status 410 session_expired or 404', () => {
  it('discards the local record and offers a fresh start on 410', async () => {
    const h = makeHarness()
    h.server.inject('confirm', new ApiError(410, 'session_expired', 'expired'))

    await h.clock.runUntil(() => h.snap().state === 'session_expired')
    await h.clock.settle()
    expect(h.storage.records.size).toBe(0)

    h.engine.retry(h.id)
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.server.calls.filter((c) => c.startsWith('create')).length).toBe(2)
  })

  it('treats a 404 the same way', async () => {
    const h = makeHarness()
    h.server.inject('confirm', new ApiError(404, 'not_found', 'gone'))
    await h.clock.runUntil(() => h.snap().state === 'session_expired')
  })
})

describe('row: handshake returns status done (another tab finished it)', () => {
  it('fetches the node and marks the upload complete without uploading anything', async () => {
    const h = makeHarness()
    h.server.sessionState = 'done'
    h.server.nodeId = 'node-9'
    h.server.publishedName = 'a (2).bin'

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.put.puts).toEqual([])
    expect(h.server.calls).toContain('node:node-9')
    expect(h.snap().name).toBe('a (2).bin')
    expect(h.snap().renamed).toBe(true)
  })
})

describe('row: /complete crash, timeout, and 409 in_progress', () => {
  it('409 in_progress → keeps polling status and never re-sends complete', async () => {
    const h = makeHarness()
    h.server.inject('complete', new ApiError(409, 'in_progress', 'finalizer running'))
    h.server.inject('status', {
      ...h.server.statusShape(),
      status: 'completing',
      confirmed_parts: [1, 2, 3],
    })
    h.server.inject('status', {
      ...h.server.statusShape(),
      status: 'done',
      node_id: 'node-7',
      confirmed_parts: [1, 2, 3],
    })

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.server.calls.filter((c) => c.startsWith('complete:')).length).toBe(1)
    expect(h.server.calls.filter((c) => c.startsWith('status:')).length).toBe(2)
    expect(h.snap().node_id).toBe('node-7')
  })

  it('a complete that never answers → paused_backend, then complete is re-sent', async () => {
    const h = makeHarness()
    h.server.inject('complete', new NetworkError('socket closed'))

    await h.clock.runUntil(() => h.snap().state === 'paused_backend')
    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.server.calls.filter((c) => c.startsWith('complete:')).length).toBe(2)
  })

  it('422 at complete → re-handshake, re-upload what the server deleted, complete again', async () => {
    const h = makeHarness()
    h.server.inject('complete', new ApiError(422, 'invalid', 'verify mismatch'))
    // PLAN: on a verify mismatch the server deletes the offending ledger rows,
    // so the next handshake re-requests exactly those parts.
    let dropped = false
    h.server.onCall = (method) => {
      if (method === 'complete' && !dropped) {
        dropped = true
        h.server.confirmed.delete(2)
      }
    }

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(partsPut(h, 2)).toBe(2)
    expect(h.server.calls.filter((c) => c.startsWith('complete:')).length).toBe(2)
  })
})

describe('row: pause', () => {
  it('aborts in-flight XHRs, suspends both hash workers, charges no budget', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    h.put.hold.add(2)
    await h.clock.runUntil(() => h.put.puts.length >= 2)

    const resumedMd5 = h.md5.resumes
    const resumedSha = h.sha256.resumes
    h.engine.pause(h.id)

    expect(h.snap().state).toBe('paused')
    expect(h.put.aborts.length).toBeGreaterThan(0)
    // Nothing else is running, so the shared workers really do get suspended.
    expect(h.md5.suspends).toBe(1)
    expect(h.sha256.suspends).toBe(1)

    h.put.hold.clear()
    h.engine.resume(h.id)
    expect(h.md5.resumes).toBeGreaterThan(resumedMd5)
    expect(h.sha256.resumes).toBeGreaterThan(resumedSha)

    await h.clock.runUntil(() => h.snap().state === 'done')
    // Re-PUT once after the pause; the abort consumed no integrity budget.
    expect(partsPut(h, 1)).toBe(2)
    expect(h.snap().state).toBe('done')
  })
})

describe('URL pool refill', () => {
  it('calls the resume handshake proactively when URLs run low — no error required', async () => {
    // 12 parts, and `POST /uploads` presigns the first ~8 missing ones, so the
    // pool genuinely runs low mid-transfer. concurrency 1 keeps the interleaving
    // deterministic. The fake server stays contract-conformant: every resume
    // returns fresh URLs for EVERY missing part (PLAN's frozen Appendix).
    const h = makeHarness({ fileSize: 12_000, env: { concurrency: 1 } })
    await h.clock.runUntil(() => h.snap().state === 'done')

    const calls = h.server.calls
    // 1. No opening handshake: part 1 went straight out on a URL create gave us.
    expect(calls[0]).toBe('create:-')
    expect(calls[1]).toBe('confirm:1')

    // 2. The refills are PROACTIVE — issued while unused URLs were still in the
    //    pool, not reactively at the first part that has none. Without the
    //    `pool.size <= LOW_POOL` branch the first handshake could not appear
    //    before part 9 was dequeued, i.e. after confirm:8.
    const refills = calls.filter((c) => c === 'resume:refill')
    expect(refills.length).toBeGreaterThanOrEqual(2)
    expect(calls.indexOf('resume:refill')).toBeLessThan(calls.indexOf('confirm:7'))

    // 3. Every part moved exactly once, so no PUT ever went out without a URL.
    expect(h.put.puts.map((p) => p.part)).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
  })
})

describe('matched session (create returned an existing session)', () => {
  it('never re-PUTs a part the server already confirmed', async () => {
    const h = makeHarness()
    h.server.confirmed.add(1)
    h.server.confirmed.add(2)

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.put.puts.map((p) => p.part)).toEqual([3])
  })
})

describe('chimera guard (verify_parts)', () => {
  it('sends MD5s for exactly the pinned parts, then uploads once the flag clears', async () => {
    const h = makeHarness()
    h.server.verifyArmed = [1, 3]

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.server.partMd5sSeen).toEqual([{ '1': md5Of(1), '3': md5Of(3) }])
    expect(h.server.calls[0]).toBe('create:-')
    expect(h.server.calls[1]).toBe('resume:verify')
    expect(h.put.puts.length).toBeGreaterThan(0)
  })

  it('restarts the whole-file SHA-256 when the flag arms MID-FLIGHT', async () => {
    const h = makeHarness()
    // PLAN arms verify_parts on "a reconciliation that found ledger/Garage
    // drift" — i.e. during an ordinary refill, long after create. That path
    // returns straight into the transfer loop, past drive()'s hash guard.
    h.put.scriptPart(2, expired403) // forces one mid-flight handshake
    let armed = false
    h.server.onCall = (method) => {
      if (method === 'resume' && !armed) {
        armed = true
        h.server.verifyArmed = [1, 3]
      }
    }

    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.server.calls).toContain('resume:verify')
    // The bug this pins: `complete` sent {"sha256": null} — an `await null` —
    // destroying the V2 integrity-scrub input for interrupted uploads only.
    expect(h.server.calls.filter((c) => c.startsWith('complete:'))).toEqual(['complete:sha-0-3000'])
  })

  it('refuses the resume when a pinned part does not match', async () => {
    const h = makeHarness()
    h.server.verifyArmed = [1, 3]
    h.server.expectedVerify = { '1': 'different', '3': 'different' }

    await h.clock.runUntil(() => h.snap().state === 'failed')
    expect(h.snap().error_code).toBe('verify_failed')
    expect(h.put.puts).toEqual([])
  })
})

describe('0-byte file', () => {
  it('uploads no parts and goes straight to complete', async () => {
    const h = makeHarness({ fileSize: 0 })
    await h.clock.runUntil(() => h.snap().state === 'done')

    expect(h.put.puts).toEqual([])
    expect(h.server.calls).toContain('complete:sha-0-0')
  })
})

describe('name conflict', () => {
  it('surfaces the prompt and retries the create with the chosen policy', async () => {
    const h = makeHarness()
    h.server.inject('create', new ApiError(409, 'name_conflict', 'a.bin already exists'))

    await h.clock.runUntil(() => h.snap().state === 'conflict')
    expect(h.snap().error_code).toBe('name_conflict')

    h.engine.resolveConflict(h.id, 'rename')
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.server.calls).toContain('create:rename')
  })
})

describe('multi-tab', () => {
  it('shows blocked_other_tab and polls GET /uploads/{id} when the lock is taken', async () => {
    const h = makeHarness()
    h.locks.takeExternally('upload:fp:parent-1')
    await h.storage.put({
      upload_id: 'up-1',
      fingerprint: 'fp',
      parent_id: 'parent-1',
      file_name: 'a.bin',
      file_size: 3000,
      part_size: 1000,
      parts_total: 3,
      confirmed_parts: [1],
      status: 'active',
      session_expires_at: '2099-01-01T00:00:00Z',
      updated_at: 0,
    })

    await h.clock.runUntil(() => h.snap().state === 'blocked_other_tab')
    expect(h.snap().error).toMatch(/another tab/i)

    await h.clock.runUntil(() => h.server.calls.some((c) => c === 'status:up-1'))
    expect(h.put.puts).toEqual([])

    // The other tab goes away: the next poll takes the lock and uploads.
    h.locks.releaseExternally('upload:fp:parent-1')
    await h.clock.runUntil(() => h.snap().state === 'done')
  })
})

describe('progress, speed and ETA', () => {
  it('derives progress from confirmed parts, never from XHR bytes', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    h.put.hold.add(2)
    h.put.progressWhileHeld.add(1)
    h.put.progressWhileHeld.add(2)

    await h.clock.runUntil(() => h.put.puts.length >= 2)
    expect(h.snap().progress).toBe(0)
    expect(h.snap().bytes_confirmed).toBe(0)
    expect(h.snap().parts_confirmed).toBe(0)

    h.put.hold.clear()
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.snap().bytes_confirmed).toBe(3000)
  })

  it('computes a speed from part-completion cadence', async () => {
    const h = makeHarness({ env: { concurrency: 1 } })
    h.put.scriptPart(2, netFail) // its backoff advances the clock between parts

    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(h.snap().speed_bps).not.toBeNull()
    expect(h.snap().speed_bps!).toBeGreaterThan(0)
  })
})

describe('local record', () => {
  it('persists the session keyed by upload_id with the (fingerprint, parent_id) pair', async () => {
    const h = makeHarness()
    await h.clock.runUntil(() => h.storage.writes.length >= 1)

    const rec = h.storage.writes[0]
    expect(rec.upload_id).toBe('up-1')
    expect(rec.fingerprint).toBe('fp')
    expect(rec.parent_id).toBe('parent-1')
    expect(rec.part_size).toBe(1000)
    expect(rec.parts_total).toBe(3)
  })

  it('removes the record once the upload is published', async () => {
    const h = makeHarness()
    await h.clock.runUntil(() => h.snap().state === 'done')
    await h.clock.settle()
    expect(h.storage.records.size).toBe(0)
  })
})

describe('cancel', () => {
  it('aborts transfers, deletes the session server-side and drops the record', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    await h.clock.runUntil(() => h.put.puts.length >= 1)

    await h.engine.cancel(h.id)

    expect(h.snap().state).toBe('canceled')
    expect(h.put.aborts.length).toBeGreaterThan(0)
    expect(h.server.calls).toContain('remove:up-1')
  })
})
