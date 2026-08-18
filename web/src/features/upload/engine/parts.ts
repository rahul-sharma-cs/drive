/**
 * Pure part math: slicing, ETag normalization, PUT-failure classification,
 * the file-changed probe, and the fingerprint recipe.
 *
 * No I/O beyond reading the user's own File, no engine state.
 */

import type { HashLike } from './types'

/** Fingerprint edge-block size: the first and last 1 MiB of the file. */
export const EDGE_BYTES = 1024 * 1024

export interface PartRange {
  start: number
  end: number
  size: number
}

/** 1-based part `n` of a file sliced at `partSize`. */
export function partRange(n: number, partSize: number, fileSize: number): PartRange {
  const start = (n - 1) * partSize
  const end = Math.min(start + partSize, fileSize)
  return { start, end, size: end - start }
}

export function partCount(fileSize: number, partSize: number): number {
  return Math.ceil(fileSize / partSize)
}

/**
 * S3/Garage return `ETag: "hex"`; some proxies add a `W/` weak prefix.
 * An unnormalized compare always fails and would falsely downgrade integrity.
 * Normalized, a plain part's ETag equals the client's own MD5 hex — confirmed
 * against Garage in the day-0 spike.
 */
export function normalizeEtag(raw: string | null | undefined): string {
  if (!raw) return ''
  return raw.trim().replace(/^W\//i, '').replace(/^"|"$/g, '').toLowerCase()
}

export type PutFailure = 'expired' | 'network' | 'hard'

/**
 * Classify a failed part PUT.
 *
 * `expired` — 403 **or** 400 carrying `<Code>InvalidRequest</Code>`, which is
 * what Garage actually answers for an expired presign (spike-measured
 * 2026-08-14, fixture in e2e/fixtures/spike/spike-report.json). A plain 400
 * without that code stays a hard failure.
 *
 * `network` — status 0 (no response reached JS: dropped socket, CORS-opaque
 * error, or an expired-URL body the browser refused to expose) and 5xx.
 *
 * `hard` — everything else; goes through the integrity budget so it is bounded.
 */
export function classifyPutStatus(status: number, body: string): PutFailure {
  if (status === 403) return 'expired'
  if (status === 400 && /<Code>\s*InvalidRequest\s*<\/Code>/i.test(body)) return 'expired'
  if (status === 0) return 'network'
  if (status >= 500) return 'network'
  return 'hard'
}

/** A slice read that failed because the file moved/changed under us. */
export function isFileChangedError(err: unknown): boolean {
  const name = (err as { name?: string } | null)?.name ?? ''
  const message = String((err as { message?: string } | null)?.message ?? err ?? '')
  return (
    name === 'NotReadableError' ||
    name === 'NotFoundError' ||
    /NotReadableError|NotFoundError/i.test(message)
  )
}

/**
 * Probe with a 1-byte `file.slice(0,1).arrayBuffer()` — the cheapest way to
 * tell a file that changed on disk from a network failure, since both surface
 * as an unhelpful read error.
 * Resolves true when the file is still readable. A 0-byte file has nothing to
 * read, so an empty slice that resolves is still proof the handle is alive.
 */
export async function probeFile(file: Blob): Promise<boolean> {
  try {
    await file.slice(0, 1).arrayBuffer()
    return true
  } catch {
    return false
  }
}

/**
 * `sha256(name + size + lastModified + sha256(first 1MiB) + sha256(last 1MiB))`
 *
 * Encoding pinned — the Go `uploadclient` MUST match this byte for byte or
 * resume silently breaks:
 *   - concatenation with NO separators, UTF-8 encoded;
 *   - `size` and `lastModified` are base-10 integers, `lastModified` in
 *     **milliseconds** since the epoch (Go must truncate an ns mtime to ms);
 *   - both edge digests are **lowercase hex** sha256;
 *   - the result is lowercase hex sha256;
 *   - a file below 1 MiB hashes the whole file twice (both edges clamp to it).
 */
export async function computeFingerprint(
  file: File,
  sha256: HashLike,
  edge = EDGE_BYTES,
): Promise<string> {
  const size = file.size
  const head = await sha256.hash(file, 0, Math.min(edge, size))
  const tail = await sha256.hash(file, Math.max(0, size - edge), size)
  const payload = `${file.name}${size}${file.lastModified}${head}${tail}`
  return sha256.hash(new Blob([payload]))
}
