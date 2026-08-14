/**
 * Shared test fakes for the upload engine, plus a sanity suite for the fakes
 * themselves.
 *
 * They live in a `*.test.ts` file because that glob is this agent's ownership
 * boundary; every other engine test imports from here.
 */

import { describe, expect, it } from 'vitest'
import { UploadEngine, type EngineEnv } from '../engine'
import type { RecordStorage, UploadRecord } from '../idb'
import { partRange } from '../parts'
import {
  ApiError,
  type Clock,
  type CompleteResponse,
  type ConfirmResponse,
  type CreateRequest,
  type CreateResponse,
  type HashLike,
  type LockManagerLike,
  type NodeSummary,
  type PartPutter,
  type PartTransfer,
  type PutOutcome,
  type ResumeResponse,
  type SessionState,
  type SessionStatus,
  type TimerHandle,
  type UploadApi,
  type UploadSnapshot,
} from '../types'

/** Garage's real answer to an expired presigned URL (spike-measured). */
export const EXPIRED_BODY =
  '<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidRequest</Code>' +
  '<Message>Bad request: Date is too old</Message><Resource>/drive-blobs/o</Resource>' +
  '<Region>garage</Region></Error>'

export const flush = (): Promise<void> => new Promise<void>((r) => globalThis.setTimeout(r, 0))

export class FakeClock implements Clock {
  private t = 0
  private next = 1
  private tasks = new Map<number, { at: number; fn: () => void }>()

  now(): number {
    return this.t
  }

  setTimeout(fn: () => void, ms: number): TimerHandle {
    const handle = this.next++
    this.tasks.set(handle, { at: this.t + Math.max(0, ms), fn })
    return handle
  }

  clearTimeout(handle: TimerHandle): void {
    this.tasks.delete(handle)
  }

  /** Longest wait currently scheduled — used to assert the 60 s cap. */
  get pendingDelays(): number[] {
    return [...this.tasks.values()].map((t) => t.at - this.t)
  }

  private fireNext(): boolean {
    let pick: [number, { at: number; fn: () => void }] | null = null
    for (const entry of this.tasks) {
      if (!pick || entry[1].at < pick[1].at) pick = entry
    }
    if (!pick) return false
    this.tasks.delete(pick[0])
    this.t = Math.max(this.t, pick[1].at)
    pick[1].fn()
    return true
  }

  /** Drives microtasks and timers until `pred` holds. Throws if it never does. */
  async runUntil(pred: () => boolean, limit = 4000): Promise<void> {
    let idle = 0
    for (let i = 0; i < limit; i++) {
      await flush()
      if (pred()) return
      if (!this.fireNext()) {
        if (++idle > 4) break
      } else {
        idle = 0
      }
    }
    await flush()
    if (pred()) return
    throw new Error('runUntil: condition never became true')
  }

  /**
   * Moves the clock forward, firing everything due on the way.
   *
   * An engine that is mid-async-chain has no timer pending for a few macrotask
   * turns, so "nothing is due right now" is not "nothing is left": keep
   * flushing a few more times before concluding the run is over, exactly as
   * `runUntil` does. Without that, advancing 5 minutes silently simulates far
   * less than 5 minutes of engine work.
   */
  async advance(ms: number): Promise<void> {
    const target = this.t + Math.max(0, ms)
    const due = (): [number, { at: number; fn: () => void }] | null => {
      let pick: [number, { at: number; fn: () => void }] | null = null
      for (const entry of this.tasks) {
        if (entry[1].at <= target && (!pick || entry[1].at < pick[1].at)) pick = entry
      }
      return pick
    }
    for (;;) {
      await flush()
      let pick = due()
      for (let idle = 0; !pick && idle < 4; idle++) {
        await flush()
        pick = due()
      }
      if (!pick) break
      this.tasks.delete(pick[0])
      this.t = Math.max(this.t, pick[1].at)
      pick[1].fn()
    }
    this.t = Math.max(this.t, target)
    await flush()
  }

  /** Runs pending work without a goal (used after terminal transitions). */
  async settle(rounds = 30): Promise<void> {
    for (let i = 0; i < rounds; i++) {
      await flush()
      if (!this.fireNext()) return
    }
  }
}

/**
 * Models ONE hash worker, faithfully including the part that bites: `suspend`
 * sets a MODULE-GLOBAL flag in the real worker, so a suspended worker blocks
 * EVERY job — its owner's and every other machine's — until `resume`.
 */
export class FakeHash implements HashLike {
  suspends = 0
  resumes = 0
  calls: { start: number; end: number }[] = []
  fail: Error | null = null
  private readonly fn: (start: number, end: number, blob: Blob) => string
  private gate: Promise<void> | null = null
  private open: (() => void) | null = null

  constructor(fn: (start: number, end: number, blob: Blob) => string) {
    this.fn = fn
  }

  async hash(blob: Blob, start = 0, end = blob.size): Promise<string> {
    this.calls.push({ start, end })
    await flush()
    if (this.gate) await this.gate
    if (this.fail) {
      const err = this.fail
      this.fail = null
      throw err
    }
    return this.fn(start, end, blob)
  }

  suspend(): void {
    this.suspends++
    if (!this.gate) this.gate = new Promise<void>((resolve) => (this.open = resolve))
  }

  resume(): void {
    this.resumes++
    this.open?.()
    this.open = null
    this.gate = null
  }
}

export class FakeStorage implements RecordStorage {
  readonly records = new Map<string, UploadRecord>()
  writes: UploadRecord[] = []

  async put(record: UploadRecord): Promise<void> {
    this.writes.push({ ...record })
    this.records.set(record.upload_id, { ...record })
  }

  async remove(uploadId: string): Promise<void> {
    this.records.delete(uploadId)
  }

  async find(fingerprint: string, parentId: string): Promise<UploadRecord | null> {
    for (const r of this.records.values()) {
      if (r.fingerprint === fingerprint && r.parent_id === parentId) return r
    }
    return null
  }
}

export class FakeLocks implements LockManagerLike {
  readonly held = new Set<string>()
  readonly requested: string[] = []

  /** Simulates another tab already holding the lock. */
  takeExternally(name: string): void {
    this.held.add(name)
  }

  releaseExternally(name: string): void {
    this.held.delete(name)
  }

  async request(
    name: string,
    _options: { ifAvailable: true },
    callback: (lock: unknown) => Promise<void>,
  ): Promise<void> {
    this.requested.push(name)
    if (this.held.has(name)) {
      await callback(null)
      return
    }
    this.held.add(name)
    try {
      await callback({ name })
    } finally {
      this.held.delete(name)
    }
  }
}

/** Deterministic per-part MD5 the fake Garage echoes back as an ETag. */
export const md5Of = (part: number): string => `md5part${part}`

export class FakeServer implements UploadApi {
  readonly calls: string[] = []
  readonly confirmed = new Set<number>()
  readonly partMd5sSeen: Record<string, string>[] = []
  readonly injected = new Map<string, unknown[]>()

  uploadId = 'up-1'
  sessionState: SessionState = 'active'
  nodeId: string | null = null
  publishedName: string
  verifyArmed: number[] | null = null
  expectedVerify: Record<string, string> | null = null
  urlSeq = 0
  /** Fires at the start of every call — lets a test mutate server state. */
  onCall: ((method: string) => void) | null = null

  constructor(
    readonly fileSize: number,
    readonly partSize: number,
    readonly fileName = 'a.bin',
    readonly parentId = 'parent-1',
  ) {
    this.publishedName = fileName
  }

  get partsTotal(): number {
    return Math.ceil(this.fileSize / this.partSize)
  }

  /** Queue results/errors for the next calls of `method`. */
  inject(method: keyof UploadApi, ...items: unknown[]): void {
    const q = this.injected.get(method) ?? []
    q.push(...items)
    this.injected.set(method, q)
  }

  private take(method: string): unknown {
    this.onCall?.(method)
    const q = this.injected.get(method)
    if (!q || q.length === 0) return undefined
    const item = q.shift()
    if (item instanceof Error) throw item
    return item ?? undefined
  }

  missing(): number[] {
    const out: number[] = []
    for (let n = 1; n <= this.partsTotal; n++) if (!this.confirmed.has(n)) out.push(n)
    return out
  }

  url(part: number): string {
    return `https://garage.test/o?part=${part}&sig=${++this.urlSeq}`
  }

  private presigned(parts: number[]) {
    return parts.map((n) => ({ part_number: n, url: this.url(n), expires_at: '2099-01-01T00:00:00Z' }))
  }

  statusShape(): SessionStatus {
    return {
      upload_id: this.uploadId,
      mode: 'direct',
      file_name: this.fileName,
      file_size: this.fileSize,
      part_size: this.partSize,
      parts_total: this.partsTotal,
      fingerprint: 'fp',
      parent_id: this.parentId,
      status: this.sessionState,
      confirmed_parts: [...this.confirmed].sort((a, b) => a - b),
      node_id: this.nodeId,
      session_expires_at: '2099-01-01T00:00:00Z',
    }
  }

  async create(req: CreateRequest): Promise<CreateResponse> {
    this.calls.push(`create:${req.conflict_policy ?? '-'}`)
    const injected = this.take('create')
    if (injected) return injected as CreateResponse
    if (this.verifyArmed) {
      return { ...this.statusShape(), presigned: [], verify_parts: this.verifyArmed }
    }
    return { ...this.statusShape(), presigned: this.presigned(this.missing().slice(0, 8)) }
  }

  async status(uploadId: string): Promise<SessionStatus> {
    this.calls.push(`status:${uploadId}`)
    const injected = this.take('status')
    if (injected) return injected as SessionStatus
    return this.statusShape()
  }

  async resume(_uploadId: string, partMd5s?: Record<string, string>): Promise<ResumeResponse> {
    this.calls.push(`resume:${partMd5s ? 'verify' : 'refill'}`)
    const injected = this.take('resume')
    if (injected) return injected as ResumeResponse
    if (this.verifyArmed) {
      if (!partMd5s) return { verify_parts: this.verifyArmed }
      this.partMd5sSeen.push(partMd5s)
      const want = this.expectedVerify ?? Object.fromEntries(this.verifyArmed.map((n) => [String(n), md5Of(n)]))
      const ok =
        Object.keys(want).length === Object.keys(partMd5s).length &&
        Object.entries(want).every(([k, v]) => partMd5s[k] === v)
      if (!ok) throw new ApiError(409, 'invalid', 'part verification failed')
      this.verifyArmed = null
    }
    return { ...this.statusShape(), missing: this.presigned(this.missing()) }
  }

  async confirm(
    _uploadId: string,
    partNumber: number,
    body: { etag: string; md5: string; size: number },
  ): Promise<ConfirmResponse> {
    this.calls.push(`confirm:${partNumber}`)
    const injected = this.take('confirm')
    if (injected) return injected as ConfirmResponse
    if (body.etag !== body.md5) throw new ApiError(422, 'invalid', 'etag mismatch')
    this.confirmed.add(partNumber)
    return { confirmed: true, session_expires_at: '2099-01-01T00:00:00Z' }
  }

  async complete(_uploadId: string, sha256: string): Promise<CompleteResponse> {
    this.calls.push(`complete:${sha256}`)
    const injected = this.take('complete')
    if (injected) return injected as CompleteResponse
    this.sessionState = 'done'
    this.nodeId = 'node-1'
    return { node_id: 'node-1', name: this.publishedName }
  }

  async remove(uploadId: string): Promise<void> {
    this.calls.push(`remove:${uploadId}`)
  }

  async node(nodeId: string): Promise<NodeSummary> {
    this.calls.push(`node:${nodeId}`)
    const injected = this.take('node')
    if (injected) return injected as NodeSummary
    return { id: nodeId, name: this.publishedName, parent_id: this.parentId }
  }
}

export class FakePut {
  readonly puts: { part: number; url: string; size: number }[] = []
  readonly aborts: number[] = []
  /** Per-part queues of scripted outcomes; empty ⇒ a clean 200. */
  readonly script = new Map<number, PutOutcome[]>()
  /** Parts whose PUT never completes on its own (stall / pause tests). */
  readonly hold = new Set<number>()
  /** Parts whose held PUT should still emit progress events. */
  readonly progressWhileHeld = new Set<number>()
  /** Parts whose held PUT keeps emitting progress every N ms of fake time. */
  private readonly beats = new Map<number, number>()

  /** Needs the fake clock to schedule heartbeats — never a real timer. */
  constructor(private readonly clock?: FakeClock) {}

  scriptPart(part: number, ...outcomes: PutOutcome[]): void {
    const q = this.script.get(part) ?? []
    q.push(...outcomes)
    this.script.set(part, q)
  }

  /**
   * A slow-but-alive part: a 100 MiB part on a 2 Mbit link takes ~7 minutes and
   * emits progress the whole time. The watchdog must be re-armed by it.
   */
  heartbeat(part: number, everyMs: number): void {
    this.hold.add(part)
    this.beats.set(part, everyMs)
  }

  readonly putter: PartPutter = (url, body, onProgress): PartTransfer => {
    const part = Number(new URL(url).searchParams.get('part'))
    this.puts.push({ part, url, size: body.size })
    let settle!: (o: PutOutcome) => void
    const promise = new Promise<PutOutcome>((resolve) => {
      settle = resolve
    })
    const scripted = this.script.get(part)?.shift()
    let beat: number | null = null
    if (this.hold.has(part)) {
      if (this.progressWhileHeld.has(part)) queueMicrotask(() => onProgress(1))
      const every = this.beats.get(part)
      if (every !== undefined && this.clock) {
        let sent = 0
        const tick = (): void => {
          onProgress((sent += 1))
          beat = this.clock!.setTimeout(tick, every)
        }
        beat = this.clock.setTimeout(tick, every)
      }
    } else {
      queueMicrotask(() => {
        onProgress(body.size)
        settle(scripted ?? { kind: 'response', status: 200, etag: `"${md5Of(part)}"`, body: '' })
      })
    }
    return {
      promise,
      abort: () => {
        if (beat !== null) this.clock?.clearTimeout(beat)
        this.aborts.push(part)
        settle({ kind: 'aborted' })
      },
    }
  }
}

export interface Harness {
  engine: UploadEngine
  server: FakeServer
  put: FakePut
  clock: FakeClock
  md5: FakeHash
  sha256: FakeHash
  storage: FakeStorage
  locks: FakeLocks
  file: File
  id: string
  snap: () => UploadSnapshot
}

export function makeFile(size: number, name = 'a.bin'): File {
  return new File([new Uint8Array(size)], name, { lastModified: 1_700_000_000_000 })
}

/**
 * A booted engine with one enqueued file. `partSize`/`fileSize` are tiny so the
 * tests exercise the same code paths as a 50 GB upload without the bytes.
 */
export function makeHarness(
  opts: {
    fileSize?: number
    partSize?: number
    env?: Partial<EngineEnv>
    file?: File
    enqueue?: boolean
  } = {},
): Harness {
  const fileSize = opts.fileSize ?? 3000
  const partSize = opts.partSize ?? 1000
  const file = opts.file ?? makeFile(fileSize)
  const server = new FakeServer(fileSize, partSize, file.name)
  const clock = new FakeClock()
  const put = new FakePut(clock)
  const md5 = new FakeHash((start) => md5Of(Math.floor(start / partSize) + 1))
  // Hashing a File => `sha-<start>-<end>`; hashing the fingerprint payload blob
  // (a plain Blob) => the fixed fingerprint 'fp', so lock names are predictable.
  const sha256 = new FakeHash((start, end, blob) => (blob instanceof File ? `sha-${start}-${end}` : 'fp'))
  const storage = new FakeStorage()
  const locks = new FakeLocks()

  const env: EngineEnv = {
    api: server,
    put: put.putter,
    md5: () => md5,
    sha256: () => sha256,
    storage: () => storage,
    locks: () => locks,
    clock,
    random: () => 0.5,
    concurrency: 2,
    pollMs: 1000,
    ...opts.env,
  }
  const engine = new UploadEngine(env)
  const id = opts.enqueue === false ? '' : engine.enqueue(file, server.parentId)
  return {
    engine,
    server,
    put,
    clock,
    md5,
    sha256,
    storage,
    locks,
    file,
    id,
    snap: () => engine.getSnapshot().items.find((i) => i.id === id)!,
  }
}

/** Bytes a part covers, for assertions. */
export const sizeOfPart = (n: number, partSize: number, fileSize: number): number =>
  partRange(n, partSize, fileSize).size

describe('test harness', () => {
  it('fake clock fires timers in time order and reports pending delays', async () => {
    const clock = new FakeClock()
    const fired: string[] = []
    clock.setTimeout(() => fired.push('late'), 500)
    clock.setTimeout(() => fired.push('early'), 100)
    expect(clock.pendingDelays.sort((a, b) => a - b)).toEqual([100, 500])
    await clock.runUntil(() => fired.length === 2)
    expect(fired).toEqual(['early', 'late'])
    expect(clock.now()).toBe(500)
  })

  it('fake put resolves a clean 200 whose ETag is the part MD5', async () => {
    const put = new FakePut()
    const t = put.putter('https://garage.test/o?part=2', new Blob(['xy']), () => undefined)
    await expect(t.promise).resolves.toEqual({
      kind: 'response',
      status: 200,
      etag: `"${md5Of(2)}"`,
      body: '',
    })
    expect(put.puts[0]).toMatchObject({ part: 2, size: 2 })
  })

  it('fake locks hand out null when another tab holds the name', async () => {
    const locks = new FakeLocks()
    locks.takeExternally('upload:fp:parent-1')
    let got: unknown = 'unset'
    await locks.request('upload:fp:parent-1', { ifAvailable: true }, async (lock) => {
      got = lock
    })
    expect(got).toBeNull()
  })
})
