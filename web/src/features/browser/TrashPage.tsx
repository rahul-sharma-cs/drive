import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { listTrash, purgeNode, restoreNode } from '../../lib/api'
import { FormError, secondaryButtonClass } from '../../ui/controls'
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
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-6 py-6">
      <h1 className="text-lg font-semibold tracking-tight">Trash</h1>
      <FormError error={restore.error ?? purge.error} />
      <section className="rounded-xl border border-neutral-200 bg-white">
        {trash.isPending && <p className="p-4 text-sm text-neutral-500">Loading…</p>}
        {trash.isSuccess && items.length === 0 && <p className="p-4 text-sm text-neutral-500">The trash is empty.</p>}
        <ul>
          {items.map((node) => (
            <li
              key={node.id}
              className="flex items-center gap-3 border-b border-neutral-100 px-4 py-2.5 last:border-b-0"
            >
              <span className="text-sm">{node.name}</span>
              <span className="ml-auto text-xs text-neutral-500">
                {node.size === null ? 'Folder' : formatBytes(node.size)}
              </span>
              <button className={secondaryButtonClass} onClick={() => restore.mutate(node.id)}>
                Restore
              </button>
              <button className={secondaryButtonClass} onClick={() => purge.mutate(node.id)}>
                Delete forever
              </button>
            </li>
          ))}
        </ul>
      </section>
      <p className="text-xs text-neutral-500">
        Restoring puts a node back where it came from. If something already has its name there, it comes back renamed.
      </p>
    </main>
  )
}
