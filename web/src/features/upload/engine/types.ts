/**
 * Upload engine — shared types.
 *
 * Everything the engine touches from the outside world (network, clock, RNG,
 * IndexedDB, Web Locks, hashing) arrives through an injected dependency so the
 * whole state machine runs deterministically under vitest in a node env.
 *
 * The wire shapes are a frozen contract shared with the Go client in
 * server/internal/uploadclient; both speak it verbatim.
 */

/* ------------------------------------------------------------------ wire */

export type ConflictPolicy = 'replace' | 'rename' | 'reuse'

export type SessionState = 'active' | 'completing' | 'done' | 'aborted'

/** The status shape (`GET /uploads/{id}`). */
export interface SessionStatus {
  upload_id: string
  mode: string
  file_name: string
  file_size: number
  part_size: number
  parts_total: number
  fingerprint: string
  parent_id: string
  status: SessionState
  confirmed_parts: number[]
  node_id?: string | null
  session_expires_at: string
}

export interface PresignedPart {
  part_number: number
  url: string
  expires_at: string
}

/** `POST /uploads` — status shape + first ~8 missing URLs (or [] when armed). */
export interface CreateResponse extends SessionStatus {
  presigned: PresignedPart[]
  verify_parts?: number[] | null
}

/** `POST /uploads/{id}/resume` — either the chimera bounce or status + missing. */
export type ResumeResponse =
  | { verify_parts: number[]; missing?: undefined }
  | (SessionStatus & { missing: PresignedPart[]; verify_parts?: number[] | null })

export interface ConfirmResponse {
  confirmed: boolean
  session_expires_at: string
}

export interface CompleteResponse {
  node_id: string
  name: string
  parent_id?: string
}

export interface CreateRequest {
  file_name: string
  file_size: number
  mime: string
  parent_id: string
  fingerprint: string
  conflict_policy?: ConflictPolicy
}

export interface NodeSummary {
  id: string
  name: string
  parent_id: string | null
}

/** Thrown for any non-2xx JSON error envelope below 500. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

/** Thrown when the API itself is unreachable — fetch rejection or 5xx. */
export class NetworkError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'NetworkError'
  }
}

export interface UploadApi {
  create(req: CreateRequest): Promise<CreateResponse>
  status(uploadId: string): Promise<SessionStatus>
  resume(uploadId: string, partMd5s?: Record<string, string>): Promise<ResumeResponse>
  confirm(
    uploadId: string,
    partNumber: number,
    body: { etag: string; md5: string; size: number },
  ): Promise<ConfirmResponse>
  complete(uploadId: string, sha256: string): Promise<CompleteResponse>
  remove(uploadId: string): Promise<void>
  node(nodeId: string): Promise<NodeSummary>
}

/* --------------------------------------------------------------- transfer */

/** Outcome of one part PUT. Non-2xx is a *resolved* response — we classify it. */
export type PutOutcome =
  | { kind: 'response'; status: number; etag: string | null; body: string }
  | { kind: 'network'; message: string }
  | { kind: 'stalled' }
  | { kind: 'aborted' }

export interface PartTransfer {
  promise: Promise<PutOutcome>
  abort(): void
}

/** `onProgress` feeds the stall watchdog only — never the progress bar. */
export type PartPutter = (url: string, body: Blob, onProgress: (loaded: number) => void) => PartTransfer

/* ------------------------------------------------------------ environment */

export type TimerHandle = number

export interface Clock {
  now(): number
  setTimeout(fn: () => void, ms: number): TimerHandle
  clearTimeout(handle: TimerHandle): void
}

/** Subset of the hash façade (`engine/hash/hash.ts`) the engine needs. */
export interface HashLike {
  hash(blob: Blob, start?: number, end?: number): Promise<string>
  suspend(): void
  resume(): void
}

/** Subset of `navigator.locks`. */
export interface LockManagerLike {
  request(
    name: string,
    options: { ifAvailable: true },
    callback: (lock: unknown) => Promise<void>,
  ): Promise<void>
}

/* -------------------------------------------------------------- snapshot */

/**
 * Client-side upload state. These are the engine's own names, not the wire's:
 * a server session is only ever `active`, `completing`, `done` or `aborted`,
 * and `done` is the one word both vocabularies share. The sentences a person
 * reads are elsewhere again, in the manager's status table.
 */
export type UploadState =
  | 'queued'
  | 'preparing' // fingerprinting / creating the session
  | 'verifying' // chimera verify_parts round trip
  | 'uploading'
  | 'completing'
  | 'done'
  | 'paused'
  | 'paused_offline'
  | 'paused_backend'
  | 'blocked_other_tab'
  | 'conflict'
  | 'session_expired'
  | 'error_file_changed'
  | 'failed'
  | 'canceled'

export type ErrorCode =
  | 'name_conflict'
  | 'not_found'
  | 'invalid'
  | 'verify_failed'
  | 'budget_exhausted'
  | 'file_changed'
  | 'session_expired'
  | 'internal'

/** One row of the upload manager. Immutable; a new object per change. */
export interface UploadSnapshot {
  /** Stable client id — survives session recreation after an expiry. */
  id: string
  upload_id: string | null
  /** Final published name once done (may be server-auto-renamed). */
  name: string
  original_name: string
  renamed: boolean
  size: number
  parent_id: string
  state: UploadState
  /** 0..1, derived from confirmed parts — never from raw XHR bytes. */
  progress: number
  bytes_confirmed: number
  parts_total: number
  parts_confirmed: number
  /** Bytes/second from part-completion cadence; null until 2 parts land. */
  speed_bps: number | null
  eta_seconds: number | null
  error_code: ErrorCode | null
  error: string | null
  session_expires_at: string | null
  node_id: string | null
  /** Pinned parts the server wants MD5s for (chimera guard), while armed. */
  verify_parts: number[] | null
}

export interface EngineSnapshot {
  items: UploadSnapshot[]
}

/* ------------------------------------------------------------- constants */

/**
 * The state machine's budgets. INTEGRITY_BUDGET counts genuine retries of a
 * part; a fresh-URL re-handshake is deliberately not charged to it, and gets
 * its own small MAX_REHANDSHAKES so a clock skew cannot loop forever.
 */
export const INTEGRITY_BUDGET = 8
export const MAX_REHANDSHAKES = 3
export const STALL_TIMEOUT_MS = 45_000
export const PARTS_IN_FLIGHT = 4
/** Refill the presigned pool proactively once this few URLs remain unused. */
export const LOW_POOL = 2
export const COMPLETE_ATTEMPTS = 3
/** Consecutive part-PUT network failures before we probe the API. */
export const NETWORK_PROBE_AFTER = 3
/**
 * Consecutive part-PUT network failures before the upload stops retrying in
 * place and rests in `paused_backend`. Without a cap a permanently failing
 * part — e.g. an expired presign whose error body CORS hides, which arrives as
 * status 0 and classifies as `network`, never `expired` — retries forever.
 */
export const NETWORK_RETRY_BUDGET = 12
