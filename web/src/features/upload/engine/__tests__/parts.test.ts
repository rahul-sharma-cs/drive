/** Pure helpers: slicing, ETag normalization, PUT classification, fingerprint. */

import { describe, expect, it } from 'vitest'
import { BACKOFF_CAP_MS, backoffDelay } from '../backoff'
import {
  classifyPutStatus,
  computeFingerprint,
  isFileChangedError,
  normalizeEtag,
  partCount,
  partRange,
  probeFile,
} from '../parts'
import type { HashLike } from '../types'
import { EXPIRED_BODY, FakeHash, makeFile } from './harness.test'

describe('part slicing', () => {
  it('slices 1-based parts with a short final part', () => {
    expect(partCount(2500, 1000)).toBe(3)
    expect(partRange(1, 1000, 2500)).toEqual({ start: 0, end: 1000, size: 1000 })
    expect(partRange(3, 1000, 2500)).toEqual({ start: 2000, end: 2500, size: 500 })
  })
})

describe('ETag normalization', () => {
  it('strips quotes and the weak prefix and lowercases', () => {
    expect(normalizeEtag('"88D56D6A80C25497C362A9EB23C90836"')).toBe('88d56d6a80c25497c362a9eb23c90836')
    expect(normalizeEtag('W/"abc"')).toBe('abc')
    expect(normalizeEtag(null)).toBe('')
  })
})

describe('PUT failure classification', () => {
  it('403 is the expired-URL signal', () => {
    expect(classifyPutStatus(403, '')).toBe('expired')
  })

  it("400 carrying Garage's InvalidRequest body is the expired-URL signal", () => {
    // Verbatim from e2e/fixtures/spike/spike-report.json (measured 2026-08-14).
    expect(classifyPutStatus(400, EXPIRED_BODY)).toBe('expired')
  })

  it('a plain 400 without that code stays a hard failure', () => {
    expect(classifyPutStatus(400, '<Error><Code>EntityTooSmall</Code></Error>')).toBe('hard')
  })

  it('status 0 (no readable response) and 5xx are network failures', () => {
    expect(classifyPutStatus(0, '')).toBe('network')
    expect(classifyPutStatus(503, '')).toBe('network')
  })
})

describe('file-changed detection', () => {
  it('recognises NotReadableError from a slice read', () => {
    const err = new Error('The requested file could not be read')
    err.name = 'NotReadableError'
    expect(isFileChangedError(err)).toBe(true)
    expect(isFileChangedError(new Error('boom'))).toBe(false)
  })

  it('the 1-byte probe succeeds on a live file', async () => {
    await expect(probeFile(makeFile(10))).resolves.toBe(true)
  })

  it('the 1-byte probe fails when the read throws', async () => {
    const dead = {
      slice: () => ({
        arrayBuffer: () => Promise.reject(Object.assign(new Error('gone'), { name: 'NotReadableError' })),
      }),
    } as unknown as Blob
    await expect(probeFile(dead)).resolves.toBe(false)
  })
})

describe('backoff', () => {
  it('grows exponentially and never exceeds the 60 s cap', () => {
    const delays = Array.from({ length: 40 }, (_, i) => backoffDelay(i + 1, () => 1))
    expect(delays[0]).toBe(1000)
    expect(delays[1]).toBe(2000)
    expect(delays[3]).toBe(8000)
    for (const d of delays) expect(d).toBeLessThanOrEqual(BACKOFF_CAP_MS)
    expect(delays[39]).toBe(BACKOFF_CAP_MS)
  })

  it('jitters within [exp/2, exp]', () => {
    expect(backoffDelay(3, () => 0)).toBe(2000)
    expect(backoffDelay(3, () => 1)).toBe(4000)
  })
})

describe('fingerprint', () => {
  it('is sha256(name + size + lastModified + sha256(head 1MiB) + sha256(tail 1MiB))', async () => {
    const file = makeFile(2048, 'report.pdf')
    const seen: string[] = []
    const hasher = new FakeHash((start, end) => `h${start}-${end}`)
    const spy = {
      hash: async (blob: Blob, start = 0, end = blob.size) => {
        if (blob === (file as Blob)) return hasher.hash(blob, start, end)
        seen.push(await blob.text())
        return 'FINGERPRINT'
      },
      suspend: () => undefined,
      resume: () => undefined,
    }
    // edge = 1024 so both edges are exercised on a small fixture.
    const fp = await computeFingerprint(file, spy, 1024)
    expect(fp).toBe('FINGERPRINT')
    expect(seen).toEqual(['report.pdf20481700000000000h0-1024h1024-2048'])
  })

  /**
   * CROSS-LANGUAGE GOLDEN VECTOR — `server/internal/uploadclient` must produce
   * this exact hex for this exact input, or resume silently breaks: the server
   * fails the active-session match on (user, fingerprint, parent_id), opens a
   * duplicate session, and re-uploads 50 GB with no error anywhere.
   *
   *   input      2048 bytes, every byte 0x00
   *   file name  "report.pdf"
   *   size       2048
   *   lastModified 1700000000000  (MILLISECONDS since the epoch, base 10 —
   *                Go must truncate FileInfo.ModTime() with .UnixMilli(),
   *                never UnixNano and never RFC3339)
   *   edge       1 MiB, so head and tail both cover the WHOLE file
   *
   *   sha256(head) = sha256(tail)
   *              = e5a00aa9991ac8a5ee3109844d84a55583bd20572ad3ffcd42792f3c36b183ad
   *   payload    = name ++ dec(size) ++ dec(lastModified) ++ hex(head) ++ hex(tail)
   *                — NO separators, UTF-8, both digests LOWERCASE HEX (not raw bytes)
   *   fingerprint= 8d64d4f47dc60e17724c5541a57afb5014f972cec889a11d5d00f4f9548d7ca5
   */
  it('golden vector: 2048 zero bytes named report.pdf @ lastModified 1700000000000', async () => {
    // A REAL SHA-256 (WebCrypto — PLAN's own named hash-wasm fallback), so this
    // pins the digest itself, not just the payload string.
    const sha256: HashLike = {
      hash: async (blob, start = 0, end = blob.size) => {
        const digest = await crypto.subtle.digest('SHA-256', await blob.slice(start, end).arrayBuffer())
        return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('')
      },
      suspend: () => undefined,
      resume: () => undefined,
    }
    const file = new File([new Uint8Array(2048)], 'report.pdf', { lastModified: 1_700_000_000_000 })

    expect(await sha256.hash(file)).toBe(
      'e5a00aa9991ac8a5ee3109844d84a55583bd20572ad3ffcd42792f3c36b183ad',
    )
    expect(await computeFingerprint(file, sha256)).toBe(
      '8d64d4f47dc60e17724c5541a57afb5014f972cec889a11d5d00f4f9548d7ca5',
    )
  })

  it('hashes the whole file for both edges when it is smaller than 1 MiB', async () => {
    const file = makeFile(100, 'tiny.txt')
    const calls: string[] = []
    const spy = {
      hash: async (blob: Blob, start = 0, end = blob.size) => {
        calls.push(`${start}-${end}`)
        return 'x'
      },
      suspend: () => undefined,
      resume: () => undefined,
    }
    await computeFingerprint(file, spy, 1024)
    expect(calls.slice(0, 2)).toEqual(['0-100', '0-100'])
  })
})
