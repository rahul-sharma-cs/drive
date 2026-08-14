/**
 * One file's upload state machine — a direct transcription of PLAN's
 * "Client engine state machine" table. UI-agnostic: it emits snapshots.
 *
 * Every transition in that table has a vitest case in `__tests__`.
 */

import { backoffDelay } from './backoff'
import type { RecordCache } from './idb'
import {
  classifyPutStatus,
  computeFingerprint,
  isFileChangedError,
  normalizeEtag,
  partRange,
  probeFile,
} from './parts'
import {
  ApiError,
  COMPLETE_ATTEMPTS,
  INTEGRITY_BUDGET,
  LOW_POOL,
  MAX_REHANDSHAKES,
  NETWORK_PROBE_AFTER,
  NETWORK_RETRY_BUDGET,
  NetworkError,
  PARTS_IN_FLIGHT,
  STALL_TIMEOUT_MS,
  type Clock,
  type ConflictPolicy,
  type CreateResponse,
  type ErrorCode,
  type HashLike,
  type LockManagerLike,
  type PartPutter,
  type PartTransfer,
  type PresignedPart,
  type PutOutcome,
  type SessionStatus,
  type TimerHandle,
  type UploadApi,
  type UploadSnapshot,
  type UploadState,
} from './types'

/** Thrown to unwind every async loop of a superseded generation. */
const STOP = Symbol('upload-stop')

export interface MachineDeps {
  api: UploadApi
  put: PartPutter
  md5: HashLike
  sha256: HashLike
  cache: RecordCache
  clock: Clock
  random: () => number
  locks: LockManagerLike
  concurrency?: number
  stallMs?: number
  /** Base delay for status polling (in_progress, other-tab, backend probe). */
  pollMs?: number
}

export interface UploadInput {
  id: string
  file: File
  parentId: string
  mime?: string
  conflictPolicy?: ConflictPolicy
}

type ResumeOk = SessionStatus & { missing: PresignedPart[] }

const ACTIVE_STATES: ReadonlySet<UploadState> = new Set<UploadState>([
  'preparing',
  'verifying',
  'uploading',
  'completing',
])

export class UploadMachine {
  readonly id: string
  /** Not readonly: `reselect()` swaps in a re-picked File (PLAN §Resume). */
  file: File
  readonly parentId: string

  private readonly deps: MachineDeps
  private readonly mime: string
  private readonly onChange: () => void
  private readonly concurrency: number
  private readonly stallMs: number
  private readonly pollMs: number

  private state: UploadState = 'queued'
  private errorCode: ErrorCode | null = null
  private errorMessage: string | null = null

  private gen = 0
  private fingerprint: string | null = null
  private conflictPolicy: ConflictPolicy | undefined
  private session: SessionStatus | null = null
  private pendingVerify: number[] | null = null
  private verifyParts: number[] | null = null

  private pool = new Map<number, PresignedPart>()
  private queue: number[] = []
  private inflight = new Set<number>()
  private confirmed = new Set<number>()

  private integrity = new Map<number, number>()
  private rehandshakes = new Map<number, number>()
  private netFails = new Map<number, number>()
  private backendAttempt = 0

  private sha256Promise: Promise<string> | null = null
  private refilling: Promise<void> | null = null
  private transfers = new Set<PartTransfer>()
  private timers = new Set<TimerHandle>()

  private samples: { t: number; bytes: number }[] = []
  private finalName: string | null = null
  private finalParent: string | null = null
  private nodeId: string | null = null

  private lockHeld = false
  private releaseLock: (() => void) | null = null

  constructor(input: UploadInput, deps: MachineDeps, onChange: () => void) {
    this.id = input.id
    this.file = input.file
    this.parentId = input.parentId
    this.mime = input.mime ?? input.file.type ?? ''
    this.conflictPolicy = input.conflictPolicy
    this.deps = deps
    this.onChange = onChange
    this.concurrency = deps.concurrency ?? PARTS_IN_FLIGHT
    this.stallMs = deps.stallMs ?? STALL_TIMEOUT_MS
    this.pollMs = deps.pollMs ?? 2000
  }

  /* ------------------------------------------------------------- public */

  start(): void {
    if (this.state === 'done' || this.state === 'canceled') return
    // The hash workers are shared by every machine (one worker pair for the
    // whole engine), so anything that starts work must un-suspend them —
    // otherwise a paused upload would freeze the ones that follow it.
    this.deps.md5.resume()
    this.deps.sha256.resume()
    const gen = this.stop()
    void this.drive(gen).catch((e) => {
      if (e === STOP || this.gen !== gen) return
      this.failWith('internal', (e as Error)?.message ?? String(e))
    })
  }

  /**
   * PLAN: pause = abort in-flight XHRs (and suspend both hash workers — that
   * half belongs to `UploadEngine.pause`, which is the only place that can see
   * whether another machine still needs the shared workers).
   *
   * `queued` is pausable too: in a 150-file folder drop 149 rows are queued,
   * and a pause the engine silently discards is worse than no pause at all.
   */
  pause(): void {
    if (
      this.state !== 'queued' &&
      !ACTIVE_STATES.has(this.state) &&
      this.state !== 'paused_backend' &&
      this.state !== 'paused_offline'
    )
      return
    this.stop()
    this.setState('paused')
  }

  resume(): void {
    if (this.state === 'done' || this.state === 'canceled') return
    // Back into the queue rather than straight into `start()`: the engine
    // admits it under `maxActive`, so resuming never runs two uploads at once.
    this.setState('queued')
  }

  /** Per-file Retry — resets every budget (PLAN: terminal failed → Retry). */
  retry(): void {
    this.integrity.clear()
    this.rehandshakes.clear()
    this.netFails.clear()
    this.backendAttempt = 0
    const stale = this.state === 'session_expired' || this.errorCode === 'verify_failed'
    this.errorCode = null
    this.errorMessage = null
    // ALWAYS drop the cached whole-file hash: if it is the thing that failed,
    // the rejected promise would otherwise survive Retry forever (`drive()`
    // only re-creates it when it is null) and Retry would be a no-op.
    this.sha256Promise = null
    // The server session is gone (or refused us): start a fresh one.
    if (stale) this.session = null
    this.start()
  }

  /**
   * PLAN §Resume / `error_file_changed`: "the user re-selects the file".
   * Hands a freshly picked `File` to this row, keeping its id and history: the
   * fingerprint is recomputed, so `POST /uploads` either matches the same
   * session (resume, chimera-verified) or opens a fresh one.
   */
  reselect(file: File): void {
    if (this.state === 'done' || this.state === 'canceled') return
    this.stop()
    this.dropLock() // the lock name embeds the OLD fingerprint
    this.file = file
    this.fingerprint = null
    this.session = null
    this.sha256Promise = null
    this.pendingVerify = null
    this.verifyParts = null
    this.pool.clear()
    this.queue = []
    this.confirmed.clear()
    this.integrity.clear()
    this.rehandshakes.clear()
    this.netFails.clear()
    this.backendAttempt = 0
    this.samples = []
    this.errorCode = null
    this.errorMessage = null
    this.setState('queued')
  }

  /** Answer to a 409 name_conflict prompt. */
  resolveConflict(policy: ConflictPolicy): void {
    this.conflictPolicy = policy
    this.errorCode = null
    this.errorMessage = null
    this.session = null
    this.start()
  }

  async cancel(): Promise<void> {
    this.stop()
    const uploadId = this.session?.upload_id
    this.setState('canceled')
    this.dropLock()
    if (uploadId) {
      await this.deps.cache.remove(uploadId).catch(() => undefined)
      await this.deps.api.remove(uploadId).catch(() => undefined)
    }
  }

  handleOffline(): void {
    if (!ACTIVE_STATES.has(this.state) && this.state !== 'paused_backend') return
    this.stop()
    this.setState('paused_offline')
  }

  handleOnline(): void {
    if (this.state === 'paused_offline') this.start()
  }

  /** Events alone are unreliable after sleep — visibility is the second chance. */
  handleVisible(): void {
    if (this.state === 'paused_offline') {
      this.start()
      return
    }
    if (this.state === 'paused_backend') {
      this.backendAttempt = 0
      this.probeBackend()
    }
  }

  snapshot(): UploadSnapshot {
    const bytes = this.bytesConfirmed()
    const size = this.file.size
    const speed = this.speed()
    return {
      id: this.id,
      upload_id: this.session?.upload_id ?? null,
      name: this.finalName ?? this.file.name,
      original_name: this.file.name,
      renamed: this.finalName !== null && this.finalName !== this.file.name,
      size,
      parent_id: this.finalParent ?? this.parentId,
      state: this.state,
      progress: this.state === 'done' ? 1 : size === 0 ? 0 : Math.min(1, bytes / size),
      bytes_confirmed: this.state === 'done' ? size : bytes,
      parts_total: this.session?.parts_total ?? 0,
      parts_confirmed: this.confirmed.size,
      speed_bps: speed,
      eta_seconds: speed && speed > 0 ? Math.max(0, (size - bytes) / speed) : null,
      error_code: this.errorCode,
      error: this.errorMessage,
      session_expires_at: this.session?.session_expires_at ?? null,
      node_id: this.nodeId,
      verify_parts: this.verifyParts,
    }
  }

  isTerminal(): boolean {
    return (
      this.state === 'done' ||
      this.state === 'canceled' ||
      this.state === 'failed' ||
      this.state === 'error_file_changed'
    )
  }

  /** Idle = not occupying one of the engine's active-upload slots. */
  isIdle(): boolean {
    return !ACTIVE_STATES.has(this.state)
  }

  /* -------------------------------------------------------------- drive */

  private async drive(gen: number): Promise<void> {
    if (!this.fingerprint) {
      this.setState('preparing')
      this.fingerprint = await computeFingerprint(this.file, this.deps.sha256)
      this.check(gen)
    }

    if (!this.lockHeld) {
      const got = await this.acquireLock()
      this.check(gen)
      if (!got) {
        this.setState('blocked_other_tab')
        this.errorMessage = 'Already uploading in another tab'
        this.scheduleTabPoll()
        throw STOP
      }
    }

    if (!this.session) await this.createSession(gen)
    if (this.pendingVerify) {
      const pinned = this.pendingVerify
      this.pendingVerify = null
      await this.runVerify(gen, pinned)
    }

    if (this.session!.status === 'aborted') {
      await this.expire()
      throw STOP
    }
    if (this.session!.status === 'done') {
      await this.adoptDone(gen, this.session!)
      return
    }
    if (this.session!.status === 'completing') {
      await this.pollCompleting(gen)
      if (this.state === 'done') return
    }

    // The whole-file SHA-256 streams in its own worker, in parallel with the
    // part uploads; complete waits on it. A chimera re-verify restarts it from
    // byte 0 of the re-selected file.
    if (!this.sha256Promise) this.hashWholeFile()

    await this.transfer(gen)
    await this.completeUpload(gen)
  }

  /**
   * Starts (or restarts) the whole-file SHA-256. Nothing awaits it until
   * `complete`, so a detached catch keeps a rejection from surfacing as an
   * unhandled rejection in the meantime — `completeUpload` still sees it.
   */
  private hashWholeFile(): Promise<string> {
    const p = this.deps.sha256.hash(this.file)
    void p.catch(() => undefined)
    this.sha256Promise = p
    return p
  }

  private async createSession(gen: number): Promise<void> {
    this.setState('preparing')
    let res: CreateResponse
    try {
      res = await this.call(gen, () =>
        this.deps.api.create({
          file_name: this.file.name,
          file_size: this.file.size,
          mime: this.mime,
          parent_id: this.parentId,
          fingerprint: this.fingerprint!,
          conflict_policy: this.conflictPolicy,
        }),
      )
    } catch (e) {
      this.check(gen)
      if (e instanceof ApiError) {
        if (e.code === 'name_conflict') {
          this.errorCode = 'name_conflict'
          this.errorMessage = e.message
          this.setState('conflict')
          throw STOP
        }
        if (e.status === 404) {
          this.failWith('not_found', 'The destination folder no longer exists.')
          throw STOP
        }
        this.failWith('invalid', e.message)
        throw STOP
      }
      throw e
    }
    this.applyStatus(res)
    this.pool = new Map(res.presigned.map((p) => [p.part_number, p]))
    this.rebuildQueue()
    if (res.verify_parts && res.verify_parts.length > 0) this.pendingVerify = res.verify_parts
  }

  /** Chimera guard: prove the re-selected file matches the pinned parts. */
  private async runVerify(gen: number, pinned: number[]): Promise<void> {
    this.setState('verifying')
    this.verifyParts = pinned
    this.sha256Promise = null
    const session = this.session!
    const md5s: Record<string, string> = {}
    for (const n of pinned) {
      const { start, end } = partRange(n, session.part_size, session.file_size)
      md5s[String(n)] = await this.deps.md5.hash(this.file, start, end)
      this.check(gen)
    }
    let res
    try {
      res = await this.sessionCall(gen, () => this.deps.api.resume(session.upload_id, md5s))
    } catch (e) {
      this.check(gen)
      if (e instanceof ApiError && e.status === 409) {
        this.verifyFailed()
        throw STOP
      }
      throw e
    }
    if (res.missing === undefined) {
      this.verifyFailed()
      throw STOP
    }
    this.verifyParts = null
    // PLAN §Resume: "the SHA-256 worker restarts from byte 0 of the re-selected
    // file, in parallel with remaining parts; complete waits on both". A verify
    // armed MID-FLIGHT returns straight into the transfer loop, past `drive()`'s
    // guard, so restarting here is the only thing that stops `complete` from
    // sending `{"sha256": null}`.
    this.hashWholeFile()
    this.applyResume(res)
  }

  private verifyFailed(): void {
    this.verifyParts = null
    this.failWith(
      'verify_failed',
      'This file no longer matches the interrupted upload — start a fresh upload.',
    )
  }

  /* ----------------------------------------------------------- transfer */

  private async transfer(gen: number): Promise<void> {
    this.setState('uploading')
    // A restart (Retry, backend probe, resume) re-derives what is left from the
    // confirmed set — the handshake is no longer what repopulates the queue.
    this.rebuildQueue()
    // `POST /uploads` already returned the first ~8 missing URLs precisely so
    // the first bytes move without a round trip (PLAN §Upload protocol); only
    // handshake when the head of the queue has no URL. `stop()` empties the
    // pool, so URLs never survive an interruption into a later attempt.
    if (this.queue.length > 0 && !this.pool.has(this.queue[0])) await this.refill(gen)
    await this.runWorkers(gen)
  }

  private async runWorkers(gen: number): Promise<void> {
    const n = Math.min(this.concurrency, this.queue.length)
    if (n <= 0) return
    await Promise.all(Array.from({ length: n }, () => this.worker(gen)))
  }

  private async worker(gen: number): Promise<void> {
    for (;;) {
      this.check(gen)
      const n = this.queue.shift()
      if (n === undefined) return
      if (this.confirmed.has(n)) continue
      // URL pool refill: proactive, no error required (PLAN §Upload protocol).
      if (this.pool.size <= LOW_POOL && this.queue.length > 0 && !this.refilling) {
        void this.refill(gen).catch(() => undefined)
      }
      await this.uploadPart(gen, n)
    }
  }

  private async uploadPart(gen: number, n: number): Promise<void> {
    this.inflight.add(n)
    try {
      for (;;) {
        this.check(gen)
        if (this.confirmed.has(n)) return
        const presigned = this.pool.get(n)
        if (!presigned) {
          await this.refill(gen)
          if (!this.pool.has(n) && !this.confirmed.has(n)) {
            throw new Error(`no presigned URL for part ${n}`)
          }
          continue
        }

        const session = this.session!
        const { start, end, size } = partRange(n, session.part_size, session.file_size)

        let md5: string
        try {
          md5 = await this.deps.md5.hash(this.file, start, end)
        } catch (e) {
          this.check(gen)
          if (isFileChangedError(e) && !(await this.stillReadable(gen))) throw STOP
          await this.networkRetry(gen, n)
          continue
        }
        this.check(gen)

        const out = await this.putWithWatchdog(this.file.slice(start, end), presigned.url)
        this.check(gen) // a pause/cancel/offline abort unwinds here: no budget charged

        if (out.kind === 'aborted') {
          await this.networkRetry(gen, n)
          continue
        }
        if (out.kind === 'stalled') {
          // 45 s with no upload progress: network failure, no integrity charge.
          await this.networkRetry(gen, n)
          continue
        }
        if (out.kind === 'network') {
          // XHR gives no reason for a status-0 error, so probe the file itself.
          if (!(await this.stillReadable(gen))) throw STOP
          await this.networkRetry(gen, n)
          continue
        }

        if (out.status >= 200 && out.status < 300) {
          this.rehandshakes.set(n, 0)
          this.netFails.set(n, 0)
          const etag = normalizeEtag(out.etag)
          if (!etag || etag !== md5) {
            await this.chargeIntegrity(gen, n)
            continue
          }
          const done = await this.confirmPart(gen, n, etag, md5, size)
          if (done) return
          continue
        }

        const failure = classifyPutStatus(out.status, out.body)
        if (failure === 'expired') {
          await this.rehandshakePart(gen, n)
          continue
        }
        this.rehandshakes.set(n, 0)
        if (failure === 'network') {
          await this.networkRetry(gen, n)
          continue
        }
        await this.chargeIntegrity(gen, n)
      }
    } finally {
      // Only our own generation may retract the claim: `stop()` already
      // cleared the set, and a newer generation may have re-claimed the part.
      if (this.gen === gen) this.inflight.delete(n)
    }
  }

  /** Returns true when the part is confirmed and the worker may move on. */
  private async confirmPart(
    gen: number,
    n: number,
    etag: string,
    md5: string,
    size: number,
  ): Promise<boolean> {
    try {
      const res = await this.sessionCall(gen, () =>
        this.deps.api.confirm(this.session!.upload_id, n, { etag, md5, size }),
      )
      this.onConfirmed(n, size, res.session_expires_at)
      return true
    } catch (e) {
      this.check(gen)
      if (e instanceof ApiError && e.status === 422) {
        // Server rejected the part — re-slice and re-PUT on the integrity budget.
        await this.chargeIntegrity(gen, n)
        return false
      }
      if (e instanceof ApiError && e.code === 'in_progress') {
        // A finalizer is already running: poll, never re-drive blindly.
        const status = await this.sessionCall(gen, () => this.deps.api.status(this.session!.upload_id))
        this.applyStatus(status)
        await this.wait(gen, this.pollMs)
        this.start()
        throw STOP
      }
      throw e
    }
  }

  private putWithWatchdog(body: Blob, url: string): Promise<PutOutcome> {
    let timer: TimerHandle | null = null
    let stalled = false
    let transfer: PartTransfer | null = null
    const clock = this.deps.clock
    const arm = () => {
      if (timer !== null) clock.clearTimeout(timer)
      timer = clock.setTimeout(() => {
        // No xhr.upload.onprogress for stallMs: the socket died silently.
        stalled = true
        transfer?.abort()
      }, this.stallMs)
    }
    transfer = this.deps.put(url, body, arm)
    this.transfers.add(transfer)
    arm()
    return transfer.promise
      .then((out) => (out.kind === 'aborted' && stalled ? ({ kind: 'stalled' } as PutOutcome) : out))
      .finally(() => {
        if (timer !== null) clock.clearTimeout(timer)
        if (transfer) this.transfers.delete(transfer)
      })
  }

  /* ------------------------------------------------------- retry paths */

  private async chargeIntegrity(gen: number, n: number): Promise<void> {
    const used = (this.integrity.get(n) ?? 0) + 1
    this.integrity.set(n, used)
    if (used >= INTEGRITY_BUDGET) {
      this.failWith('budget_exhausted', `Part ${n} failed ${used} times.`)
      throw STOP
    }
    this.pool.delete(n)
    await this.wait(gen, backoffDelay(used, this.deps.random))
    await this.refill(gen)
  }

  /** Expired presign: fresh URLs, no integrity charge, at most 3 in a row. */
  private async rehandshakePart(gen: number, n: number): Promise<void> {
    const used = (this.rehandshakes.get(n) ?? 0) + 1
    this.rehandshakes.set(n, used)
    if (used > MAX_REHANDSHAKES) {
      this.enterBackend('Upload URLs keep expiring — check this machine’s clock.')
      throw STOP
    }
    this.pool.delete(n)
    await this.wait(gen, backoffDelay(used, this.deps.random))
    await this.refill(gen)
  }

  private async networkRetry(gen: number, n: number): Promise<void> {
    const fails = (this.netFails.get(n) ?? 0) + 1
    this.netFails.set(n, fails)
    if (fails >= NETWORK_RETRY_BUDGET) {
      // A part that fails this many times in a row is not a blip. Rest instead
      // of hammering: a CORS-opaque expired presign reaches us as status 0
      // (`network`, never `expired`), so this is the only cap on that path.
      this.enterBackend('Upload stalled — retrying in the background.')
      throw STOP
    }
    this.pool.delete(n)
    await this.wait(gen, backoffDelay(fails, this.deps.random))
    if (fails % NETWORK_PROBE_AFTER === 0) {
      // If the API is down too, sessionCall raises and we land in paused_backend.
      const status = await this.sessionCall(gen, () => this.deps.api.status(this.session!.upload_id))
      this.applyStatus(status)
    }
    await this.refill(gen)
  }

  /** 1-byte probe. False ⇒ terminal error_file_changed (already applied). */
  private async stillReadable(gen: number): Promise<boolean> {
    const ok = await probeFile(this.file)
    this.check(gen)
    if (ok) return true
    this.failWith(
      'file_changed',
      'File changed on disk — re-select it to restart the upload.',
      'error_file_changed',
    )
    return false
  }

  /* ---------------------------------------------------------- handshake */

  private refill(gen: number): Promise<void> {
    if (this.refilling) return this.refilling
    const run = (async () => {
      const res = await this.sessionCall(gen, () => this.deps.api.resume(this.session!.upload_id))
      if (res.missing === undefined) {
        await this.runVerify(gen, res.verify_parts)
        return
      }
      this.applyResume(res)
    })()
    this.refilling = run.finally(() => {
      this.refilling = null
    })
    return this.refilling
  }

  private applyResume(res: ResumeOk): void {
    this.applyStatus(res)
    this.pool = new Map(res.missing.map((p) => [p.part_number, p]))
    this.rebuildQueue()
  }

  /** Every unconfirmed part that nobody is uploading right now, in order. */
  private rebuildQueue(): void {
    const total = this.session?.parts_total ?? 0
    const queue: number[] = []
    for (let n = 1; n <= total; n++) {
      if (!this.confirmed.has(n) && !this.inflight.has(n)) queue.push(n)
    }
    this.queue = queue
  }

  private applyStatus(status: SessionStatus): void {
    this.session = status
    this.confirmed = new Set(status.confirmed_parts)
    if (status.node_id) this.nodeId = status.node_id
    this.saveRecord()
    this.emit()
  }

  private onConfirmed(n: number, size: number, expiresAt: string): void {
    this.confirmed.add(n)
    this.pool.delete(n)
    this.integrity.set(n, 0)
    // Real progress: the backend is healthy, so the probe backoff starts over.
    this.backendAttempt = 0
    if (this.session) this.session = { ...this.session, session_expires_at: expiresAt }
    // Progress/speed come from part-completion cadence, never raw XHR bytes:
    // every byte is read ~3x (MD5 pre-pass, XHR body, SHA-256 worker).
    this.samples.push({ t: this.deps.clock.now(), bytes: size })
    if (this.samples.length > 8) this.samples.shift()
    this.saveRecord()
    this.emit()
  }

  /* ----------------------------------------------------------- complete */

  private async completeUpload(gen: number): Promise<void> {
    this.setState('completing')
    let sha: string
    try {
      sha = await (this.sha256Promise ?? this.hashWholeFile())
    } catch (e) {
      this.check(gen)
      if (isFileChangedError(e) && !(await this.stillReadable(gen))) throw STOP
      // PLAN §Phase 5: never surface a raw worker/wasm message to the user.
      // `retry()` drops the rejected promise, so Retry really re-hashes.
      this.failWith('internal', 'Could not finish hashing this file — retry to try again.')
      throw STOP
    }
    this.check(gen)

    for (let attempt = 1; ; attempt++) {
      try {
        const res = await this.sessionCall(gen, () => this.deps.api.complete(this.session!.upload_id, sha))
        this.finish(res.node_id, res.name, res.parent_id ?? this.parentId)
        return
      } catch (e) {
        this.check(gen)
        if (e instanceof ApiError && e.code === 'in_progress') {
          await this.pollCompleting(gen)
          if (this.state === 'done') return
          await this.refill(gen)
          await this.runWorkers(gen)
          this.setState('completing')
          continue
        }
        if (e instanceof ApiError && e.status === 422) {
          if (attempt >= COMPLETE_ATTEMPTS) {
            this.failWith('invalid', e.message)
            throw STOP
          }
          // The server deleted the offending ledger rows: re-handshake, re-send.
          await this.refill(gen)
          await this.runWorkers(gen)
          this.setState('completing')
          continue
        }
        throw e
      }
    }
  }

  /** 409 in_progress: keep polling status, do NOT re-send complete. */
  private async pollCompleting(gen: number): Promise<void> {
    for (let attempt = 1; ; attempt++) {
      await this.wait(gen, backoffDelay(attempt, this.deps.random, this.pollMs))
      const status = await this.sessionCall(gen, () => this.deps.api.status(this.session!.upload_id))
      this.applyStatus(status)
      if (status.status === 'done') {
        await this.adoptDone(gen, status)
        return
      }
      if (status.status === 'active') return
      if (status.status === 'aborted') {
        await this.expire()
        throw STOP
      }
    }
  }

  /** Another tab (or a lost complete ack) finished it: adopt the final name. */
  private async adoptDone(gen: number, status: SessionStatus): Promise<void> {
    let name = status.file_name
    let parent = status.parent_id
    const nodeId = status.node_id ?? null
    if (nodeId) {
      try {
        const node = await this.deps.api.node(nodeId)
        this.check(gen)
        name = node.name
        parent = node.parent_id ?? parent
      } catch (e) {
        if (e === STOP) throw e
        // Keep the session's name; 4b can refresh from the folder listing.
      }
    }
    this.finish(nodeId, name, parent)
  }

  private finish(nodeId: string | null, name: string, parentId: string): void {
    this.stop()
    this.nodeId = nodeId
    this.finalName = name
    this.finalParent = parentId
    this.verifyParts = null
    this.errorCode = null
    this.errorMessage = null
    const total = this.session?.parts_total ?? 0
    for (let n = 1; n <= total; n++) this.confirmed.add(n)
    const uploadId = this.session?.upload_id
    if (uploadId) void this.deps.cache.remove(uploadId).catch(() => undefined)
    this.dropLock()
    this.setState('done')
  }

  /* -------------------------------------------------------- backend/API */

  /** Any API network error: the kill-9 case — Garage PUTs may still work. */
  private enterBackend(message = 'Paused — the server is unreachable. Will resume automatically.'): void {
    this.stop()
    this.errorCode = null
    this.errorMessage = message
    this.setState('paused_backend')
    // `backendAttempt` deliberately survives: re-entering must make the probe
    // wait LONGER, or a permanent fault (clock skew) flaps at one doomed part
    // PUT per backoffDelay(1). It resets on real progress (`onConfirmed`) and
    // on an explicit user signal (`handleVisible`).
    this.scheduleProbe()
  }

  private scheduleProbe(): void {
    const ms = backoffDelay(++this.backendAttempt, this.deps.random, this.pollMs)
    const handle = this.deps.clock.setTimeout(() => {
      this.timers.delete(handle)
      this.probeBackend()
    }, ms)
    this.timers.add(handle)
  }

  private probeBackend(): void {
    if (this.state !== 'paused_backend') return
    void (async () => {
      try {
        if (this.session) {
          const status = await this.deps.api.status(this.session.upload_id)
          this.applyStatus(status)
        }
        if (this.state !== 'paused_backend') return
        // A cleared probe ends the failed attempt: the per-part counters mean
        // "consecutive failures within one attempt", so the next attempt gets
        // its full re-handshake / network budget again.
        this.rehandshakes.clear()
        this.netFails.clear()
        this.start()
      } catch (e) {
        if (e instanceof ApiError && (e.status === 410 || e.status === 404)) {
          await this.expire()
          return
        }
        this.scheduleProbe()
      }
    })()
  }

  /** Wraps a call so an unreachable API always lands in paused_backend. */
  private async call<T>(gen: number, fn: () => Promise<T>): Promise<T> {
    try {
      const res = await fn()
      this.check(gen)
      return res
    } catch (e) {
      this.check(gen)
      if (e instanceof NetworkError) {
        this.enterBackend()
        throw STOP
      }
      throw e
    }
  }

  /** As `call`, plus: 410/404 on a session endpoint means the session is gone. */
  private async sessionCall<T>(gen: number, fn: () => Promise<T>): Promise<T> {
    try {
      return await this.call(gen, fn)
    } catch (e) {
      if (e === STOP) throw e
      this.check(gen)
      if (e instanceof ApiError && (e.status === 410 || e.status === 404)) {
        await this.expire()
        throw STOP
      }
      throw e
    }
  }

  private async expire(): Promise<void> {
    this.stop()
    const uploadId = this.session?.upload_id
    this.session = null
    this.sha256Promise = null
    this.pool.clear()
    this.queue = []
    this.confirmed.clear()
    this.errorCode = 'session_expired'
    this.errorMessage = 'Session expired — start fresh.'
    this.dropLock()
    this.setState('session_expired')
    if (uploadId) await this.deps.cache.remove(uploadId).catch(() => undefined)
  }

  /* --------------------------------------------------------- multi-tab */

  private acquireLock(): Promise<boolean> {
    const name = `upload:${this.fingerprint}:${this.parentId}`
    return new Promise<boolean>((resolve) => {
      let settled = false
      const done = (v: boolean) => {
        if (settled) return
        settled = true
        resolve(v)
      }
      this.deps.locks
        .request(name, { ifAvailable: true }, async (lock) => {
          if (!lock) {
            done(false)
            return
          }
          this.lockHeld = true
          await new Promise<void>((release) => {
            this.releaseLock = () => {
              this.lockHeld = false
              this.releaseLock = null
              release()
            }
            done(true)
          })
        })
        .catch(() => done(false))
    })
  }

  private dropLock(): void {
    this.releaseLock?.()
  }

  private scheduleTabPoll(): void {
    const handle = this.deps.clock.setTimeout(() => {
      this.timers.delete(handle)
      if (this.state !== 'blocked_other_tab') return
      void (async () => {
        try {
          const rec = this.fingerprint ? await this.deps.cache.find(this.fingerprint, this.parentId) : null
          if (rec) {
            const status = await this.deps.api.status(rec.upload_id)
            this.applyStatus(status)
            if (status.status === 'done') {
              await this.adoptDone(this.gen, status)
              return
            }
          }
        } catch {
          // The other tab owns the session; keep waiting for the lock.
        }
        if (this.state === 'blocked_other_tab') this.start()
      })()
    }, this.pollMs)
    this.timers.add(handle)
  }

  /* ------------------------------------------------------------ plumbing */

  private stop(): number {
    this.gen += 1
    for (const t of this.transfers) t.abort()
    this.transfers.clear()
    this.inflight.clear()
    // Presigned URLs must not survive an interruption: after a pause of any
    // length they may be expired, and re-PUTting 100 MiB to learn that is the
    // waste the opening handshake exists to avoid.
    this.pool.clear()
    for (const h of this.timers) this.deps.clock.clearTimeout(h)
    this.timers.clear()
    this.refilling = null
    return this.gen
  }

  private check(gen: number): void {
    if (this.gen !== gen) throw STOP
  }

  private wait(gen: number, ms: number): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const handle = this.deps.clock.setTimeout(() => {
        this.timers.delete(handle)
        if (this.gen !== gen) reject(STOP)
        else resolve()
      }, ms)
      this.timers.add(handle)
    })
  }

  private failWith(code: ErrorCode, message: string, state: UploadState = 'failed'): void {
    this.stop()
    this.errorCode = code
    this.errorMessage = message
    this.dropLock()
    this.setState(state)
  }

  private setState(state: UploadState): void {
    this.state = state
    this.emit()
  }

  private emit(): void {
    this.onChange()
  }

  private saveRecord(): void {
    const s = this.session
    if (!s || !this.fingerprint) return
    this.deps.cache.save({
      upload_id: s.upload_id,
      fingerprint: this.fingerprint,
      parent_id: s.parent_id,
      file_name: s.file_name,
      file_size: s.file_size,
      part_size: s.part_size,
      parts_total: s.parts_total,
      confirmed_parts: [...this.confirmed].sort((a, b) => a - b),
      status: s.status,
      session_expires_at: s.session_expires_at,
      updated_at: this.deps.clock.now(),
    })
  }

  private bytesConfirmed(): number {
    const s = this.session
    if (!s) return 0
    let total = 0
    for (const n of this.confirmed) total += partRange(n, s.part_size, s.file_size).size
    return total
  }

  private speed(): number | null {
    if (this.samples.length < 2) return null
    const first = this.samples[0]
    const last = this.samples[this.samples.length - 1]
    const seconds = (last.t - first.t) / 1000
    if (seconds <= 0) return null
    let bytes = 0
    for (let i = 1; i < this.samples.length; i++) bytes += this.samples[i].bytes
    return bytes / seconds
  }
}
