import { CircleAlert } from 'lucide-react'
import { useRef } from 'react'

import type { UploadSession } from '../../../lib/api'
import { Button } from '@/components/ui/button'
import { formatBytes } from '../../../ui/format'

/**
 * Interrupted uploads, offered back after a reload.
 *
 * The copy asks for *the same file* on purpose: the fingerprint covers name,
 * size, mtime and both edge blocks, so a different — or regenerated — file
 * quietly starts a new upload from zero instead of resuming, and nothing about
 * that failure is visible afterwards.
 */
export function ResumableUploads({
  sessions,
  rootId,
  onPick,
  onDiscard,
}: {
  sessions: UploadSession[]
  rootId: string
  onPick: (file: File, parentId: string) => void
  onDiscard: (uploadId: string) => void
}) {
  if (sessions.length === 0) return null
  return (
    <ul>
      {sessions.map((session) => (
        <ResumableRow
          key={session.upload_id}
          session={session}
          rootId={rootId}
          onPick={onPick}
          onDiscard={onDiscard}
        />
      ))}
    </ul>
  )
}

function ResumableRow({
  session,
  rootId,
  onPick,
  onDiscard,
}: {
  session: UploadSession
  rootId: string
  onPick: (file: File, parentId: string) => void
  onDiscard: (uploadId: string) => void
}) {
  const input = useRef<HTMLInputElement>(null)
  const done = session.confirmed_parts.length
  // A purge can null the destination out from under a session; the server
  // re-parents such an upload to the root when it completes, so the row aims
  // there too rather than refusing to resume.
  const parentId = session.parent_id ?? rootId

  return (
    <li className="flex flex-col gap-2 border-b border-line bg-warn-soft/50 px-4 py-3 last:border-b-0">
      <div className="flex items-baseline gap-2">
        <span className="min-w-0 truncate text-[13px] font-medium text-ink">{session.file_name}</span>
        <span className="numeric ml-auto shrink-0 text-ink-3">{formatBytes(session.file_size)}</span>
      </div>
      <p className="flex items-start gap-1.5 text-[13px] text-warn">
        <CircleAlert aria-hidden className="mt-0.5 size-3.5 shrink-0" />
        <span>
          Interrupted — {done} of {session.parts_total} parts are already uploaded. Pick the same file to carry on from
          there.
        </span>
      </p>
      <div className="flex gap-1">
        <Button variant="outline" size="sm" onClick={() => input.current?.click()}>
          Pick the file
        </Button>
        <Button variant="ghost" size="sm" onClick={() => onDiscard(session.upload_id)}>
          Discard
        </Button>
        <input
          ref={input}
          type="file"
          aria-label={`Pick ${session.file_name} to resume`}
          className="hidden"
          onChange={(e) => {
            const file = e.target.files?.[0]
            if (file) onPick(file, parentId)
            e.target.value = ''
          }}
        />
      </div>
    </li>
  )
}
