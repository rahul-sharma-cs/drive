import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Trash2 } from 'lucide-react'

import { Button } from '@/components/ui/button'

import { listTrash, purgeNode, restoreNode } from '../../lib/api'
import { EmptyState, FormError } from '../../ui/controls'
import { FileList } from './FileList'
import { usageKey } from './queries'

/**
 * The trash: the roots that were deleted, restorable or removable for good.
 * Only roots are listed — trashing a folder takes its whole subtree with it,
 * and restoring the root brings the subtree back.
 *
 * It renders through `FileList` like every other list, but without a selection
 * or the command band: restoring and purging are still one row at a time, and
 * a band offering bulk commands that do not exist would be a promise. Names do
 * not link either — a trashed folder is not somewhere you can go.
 */
export function TrashPage() {
  const client = useQueryClient()
  const trash = useQuery({ queryKey: ['trash'], queryFn: listTrash })

  const refresh = () => {
    void client.invalidateQueries({ queryKey: ['trash'] })
    // A restored node reappears in a folder listing, and a purged one vanishes
    // from search results, so both of those caches are stale too.
    void client.invalidateQueries({ queryKey: ['children'] })
    void client.invalidateQueries({ queryKey: ['search'] })
    // Purging is what actually gives the bytes back.
    void client.invalidateQueries({ queryKey: usageKey })
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

      <FileList
        nodes={items}
        pending={trash.isPending}
        error={trash.error}
        errorText="The trash didn’t load."
        onRetry={() => void trash.refetch()}
        empty={
          <EmptyState
            icon={<Trash2 className="size-5" />}
            title="The trash is empty."
            hint="Deleted files and folders wait here until you remove them for good."
          />
        }
        linkNames={false}
        rowExtra={(node) => (
          <>
            <Button variant="outline" size="sm" onClick={() => restore.mutate(node.id)}>
              Restore
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="hover:bg-danger-soft hover:text-danger"
              onClick={() => purge.mutate(node.id)}
            >
              Delete forever
            </Button>
          </>
        )}
      />
    </main>
  )
}
