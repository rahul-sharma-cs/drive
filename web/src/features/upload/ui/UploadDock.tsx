import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { toast } from 'sonner'

import { childrenKey } from '../../browser/queries'
import type { UploadSnapshot } from '../engine/types'
import { uploadActions, useUploadItems } from './engineStore'
import { UploadManager } from './UploadManager'

/** The manager, wired to the singleton. Mounted once, in the app layout. */
export function UploadDock() {
  const items = useUploadItems()
  useCompletionBridge(items)
  return <UploadManager items={items} actions={uploadActions} />
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
      void client.invalidateQueries({ queryKey: ['uploads'] })
      toast.success(
        item.renamed
          ? `Uploaded as “${item.name}” — “${item.original_name}” was already there`
          : `Uploaded ${item.name}`,
      )
    }
  }, [items, client])
}
