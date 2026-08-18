/**
 * Which server-side upload sessions still need a file from this browser.
 *
 * A reload destroys every `File` handle, so the engine cannot carry an upload
 * across it — and it deliberately keeps no way to enumerate its IndexedDB
 * records. The server does have the list: `GET /uploads` reports every session
 * with its confirmed parts. Those become "pick the file again" rows, and
 * re-picking creates an upload whose fingerprint matches the session, which is
 * exactly the resume path.
 *
 * The dedupe is what keeps the manager honest: the moment a re-picked file is
 * enqueued, that upload exists both as an engine row and as a server session,
 * and without this it would appear twice.
 */

import type { UploadSession } from '../../../lib/api'
import type { UploadSnapshot } from '../engine/types'

export function resumableSessions(items: UploadSnapshot[], sessions: UploadSession[]): UploadSession[] {
  const live = new Set(items.map((i) => i.upload_id).filter((id): id is string => id !== null))
  return sessions.filter((s) => s.status === 'active' && !live.has(s.upload_id))
}
