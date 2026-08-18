/**
 * What each engine state says to a person.
 *
 * Honest copy, never a status code: "Paused — offline. Will resume
 * automatically.", not "Error 500". Two rules the engine's own states force:
 * `paused_offline` and `paused_backend` must read differently —
 * one is the user's connection and one is the server, and both resume on their
 * own — and `error_file_changed` has to ask for the file again rather than
 * report a failure, because re-selecting is what fixes it.
 */

import type { UploadState } from '../engine/types'
import { formatBytes } from '../../../ui/format'

export interface UploadLine {
  /** The sentence under the file name. */
  text: string
  /** True when the upload is waiting on the person, not on the network. */
  needsUser: boolean
}

export function describeUpload(item: {
  state: UploadState
  parts_confirmed: number
  parts_total: number
  speed_bps: number | null
  eta_seconds: number | null
  error: string | null
}): UploadLine {
  switch (item.state) {
    case 'queued':
      return { text: 'Waiting to start', needsUser: false }
    case 'preparing':
      return { text: 'Reading the file…', needsUser: false }
    case 'verifying':
      return { text: 'Checking this is the same file…', needsUser: false }
    case 'uploading':
      return { text: transferLine(item), needsUser: false }
    case 'completing':
      return { text: 'Finishing up…', needsUser: false }
    case 'done':
      return { text: 'Uploaded', needsUser: false }
    case 'paused':
      return { text: 'Paused', needsUser: true }
    case 'paused_offline':
      return { text: 'Paused — offline. Will resume automatically.', needsUser: false }
    case 'paused_backend':
      return { text: "Paused — can't reach the server. Will resume automatically.", needsUser: false }
    case 'blocked_other_tab':
      return { text: 'Already uploading in another tab', needsUser: false }
    case 'conflict':
      return { text: 'A file with this name is already here', needsUser: true }
    case 'session_expired':
      return { text: 'This upload expired. Start it again to upload the whole file.', needsUser: true }
    case 'error_file_changed':
      return { text: 'The file changed on disk. Pick it again to restart.', needsUser: true }
    case 'failed':
      return { text: item.error ?? 'Upload failed', needsUser: true }
    case 'canceled':
      return { text: 'Canceled', needsUser: false }
  }
}

/**
 * Progress reads from confirmed parts, never from bytes on the wire: every byte
 * is read about three times (MD5 pre-pass, the PUT body, the SHA-256 worker),
 * so a transfer-only rate overstates what is actually being stored.
 */
function transferLine(item: {
  parts_confirmed: number
  parts_total: number
  speed_bps: number | null
  eta_seconds: number | null
}): string {
  const parts = item.parts_total > 0 ? `${item.parts_confirmed}/${item.parts_total} parts` : 'Starting…'
  const speed = item.speed_bps ? ` · ${formatBytes(item.speed_bps)}/s` : ''
  const eta = item.eta_seconds !== null && item.speed_bps ? ` · ${formatDuration(item.eta_seconds)} left` : ''
  return `${parts}${speed}${eta}`
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.ceil(seconds)}s`
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`
  return `${(seconds / 3600).toFixed(1)}h`
}
