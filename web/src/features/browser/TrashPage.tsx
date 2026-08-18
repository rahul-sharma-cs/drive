import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { listTrash, purgeNode, restoreNode } from '../../lib/api'
import { Card, dangerButtonClass, EmptyState, FormError, secondaryButtonClass, SkeletonRows } from '../../ui/controls'
import { FileIcon, FolderIcon, TrashIcon } from '../../ui/icons'
import { formatBytes } from '../../ui/format'

/**
 * The trash: the roots that were deleted, restorable or removable for good.
 * Only roots are listed — trashing a folder takes its whole subtree with it,
 * and restoring the root brings the subtree back.
 */
export function TrashPage() {
  const client = useQueryClient()
  const trash = useQuery({ queryKey: ['trash'], queryFn: listTrash })

  const refresh = () => {
    void client.invalidateQueries({ queryKey: ['trash'] })
    // A restored node reappears in a folder listing, so those are stale too.
    void client.invalidateQueries({ queryKey: ['children'] })
  }
  const restore = useMutation({ mutationFn: restoreNode, onSuccess: refresh })
  const purge = useMutation({ mutationFn: purgeNode, onSuccess: refresh })

  const items = trash.data?.items ?? []

  return (
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <div>
        <h1 className="text-[17px] font-semibold tracking-tight">Trash</h1>
        <p className="text-[13px] text-ink-3">
          Restoring puts a node back where it came from. If something already has its name there, it comes back
          renamed.
        </p>
      </div>
      <FormError error={restore.error ?? purge.error} />
      <Card>
        {trash.isPending && <SkeletonRows />}
        {trash.isSuccess && items.length === 0 && (
          <EmptyState
            icon={<TrashIcon />}
            title="The trash is empty."
            hint="Deleted files and folders wait here until you remove them for good."
          />
        )}
        {items.length > 0 && (
          <ul className="divide-y divide-line">
            {items.map((node) => (
              <li
                key={node.id}
                className="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2.5 transition duration-100 hover:bg-surface-muted sm:flex-nowrap sm:px-4"
              >
                <span className="shrink-0 text-ink-3">{node.size === null ? <FolderIcon /> : <FileIcon />}</span>
                <span className="min-w-0 truncate text-sm text-ink">{node.name}</span>
                <span className="numeric ml-auto w-16 shrink-0 text-right text-ink-3">
                  {node.size === null ? 'Folder' : formatBytes(node.size)}
                </span>
                <div className="flex w-full shrink-0 justify-end gap-1 sm:w-auto">
                  <button className={secondaryButtonClass} onClick={() => restore.mutate(node.id)}>
                    Restore
                  </button>
                  <button className={dangerButtonClass} onClick={() => purge.mutate(node.id)}>
                    Delete forever
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </main>
  )
}
