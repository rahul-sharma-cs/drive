import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { toast } from 'sonner'

import { discardUpload, listUploads } from '../../../lib/api'
import { useSession } from '../../auth/session'
import { childrenKey } from '../../browser/queries'
import type { ConflictPolicy, UploadSnapshot } from '../engine/types'
import { ConflictDialog } from './ConflictDialog'
import { uploadActions, useUploadItems } from './engineStore'
import { resumableSessions } from './resumable'
import { ResumableUploads } from './ResumableUploads'
import { UploadManager } from './UploadManager'

export const uploadsKey = ['uploads'] as const

/** The manager, wired to the singleton. Mounted once, in the app layout. */
export function UploadDock() {
  const items = useUploadItems()
  const session = useSession()
  const client = useQueryClient()
  useCompletionBridge(items)

  // Fetched once per load: this exists to answer "what was I uploading before
  // the page went away", which does not change while the tab is open except
  // through this engine — and the completion bridge invalidates it then.
  const uploads = useQuery({ queryKey: uploadsKey, queryFn: listUploads, staleTime: Infinity })
  const sessions = resumableSessions(items, uploads.data?.items ?? [])

  const discard = useMutation({
    mutationFn: discardUpload,
    onSuccess: () => client.invalidateQueries({ queryKey: uploadsKey }),
  })

  const conflicts = items.filter((i) => i.state === 'conflict')

  return (
    <>
      <UploadManager
        items={items}
        actions={uploadActions}
        resumable={
          sessions.length > 0 ? (
            <ResumableUploads
              sessions={sessions}
              rootId={session.root_id}
              onPick={(file, parentId) => uploadActions.enqueue(file, parentId)}
              onDiscard={(uploadId) => discard.mutate(uploadId)}
            />
          ) : null
        }
      />
      <ConflictDialog
        conflicts={conflicts}
        onResolve={(ids, policy: ConflictPolicy) => ids.forEach((id) => uploadActions.resolveConflict(id, policy))}
        // Skip is client-side only — PLAN §Conflict rules: it sends no request,
        // it just stops trying to upload this file.
        onSkip={(ids) => ids.forEach((id) => uploadActions.cancel(id))}
      />
    </>
  )
}

/**
 * The only thing connecting a finished upload to the file browser.
 *
 * The engine knows nothing about React Query, so when a file publishes, the
 * folder it landed in has to be re-read or the new row simply never appears.
 * The transition is detected by diffing against the previous state per row —
 * the snapshot has no events — and the adopted name is the server's final one,
 * which may differ from what was picked (PLAN §Complete auto-renames on a
 * collision), hence the badge.
 */
export function useCompletionBridge(items: UploadSnapshot[]): void {
  const client = useQueryClient()
  const seen = useRef(new Map<string, string>())

  useEffect(() => {
    for (const item of items) {
      const previous = seen.current.get(item.id)
      seen.current.set(item.id, item.state)
      if (previous === item.state || item.state !== 'done') continue
      void client.invalidateQueries({ queryKey: childrenKey(item.parent_id) })
      void client.invalidateQueries({ queryKey: uploadsKey })
      toast.success(
        item.renamed
          ? `Uploaded as “${item.name}” — “${item.original_name}” was already there`
          : `Uploaded ${item.name}`,
      )
    }
  }, [items, client])
}
