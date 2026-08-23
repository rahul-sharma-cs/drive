import { useInfiniteQuery } from '@tanstack/react-query'
import { ArchiveRestore, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'

import { listTrash, type DriveNode } from '../../lib/api'
import { EmptyState } from '../../ui/controls'
import type { BandAction } from './CommandBand'
import { EmptyTrashDialog } from './EmptyTrashDialog'
import { FileList } from './FileList'
import type { Action } from './RowMenu'
import { useEmptyTrash, usePurgeNodes, useRestoreNodes, type BulkOutcome } from './queries'

/**
 * The trash: the roots that were deleted, restorable or removable for good.
 * Only roots are listed — trashing a folder takes its whole subtree with it,
 * and restoring the root brings the subtree back.
 *
 * It is the same list as everywhere else, with three differences. The date
 * column shows when a row was thrown away rather than when it last changed,
 * which is the only date anyone comes here looking for. Names are not links: a
 * trashed folder is not somewhere you can go. And the band's idle side carries
 * Empty trash, because the whole point of this screen is leaving it empty.
 *
 * The commands act on whole selections through the bulk routes, one row
 * included — a single Restore is a selection of one, so there is one code path
 * and one way for a conflict to be reported.
 *
 * The listing pages, like a folder's. Before it did not, and the two things
 * that follow from that were both wrong quietly: "Select all N loaded" covered
 * whatever one answer happened to hold, and the empty-trash confirmation
 * offered to delete "all N items" while N was only the first page of them.
 */
export function TrashPage() {
  // Same key as before — every mutation below and in `queries.ts` invalidates
  // `['trash']`, and an infinite query answers to it just as the single-page
  // one did.
  const trash = useInfiniteQuery({
    queryKey: ['trash'],
    queryFn: ({ pageParam }) => listTrash(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
  const restore = useRestoreNodes()
  const purge = usePurgeNodes()
  const empty = useEmptyTrash()
  const [confirming, setConfirming] = useState(false)

  const items = trash.data?.pages.flatMap((page) => page.items) ?? []
  const busy = restore.isPending || purge.isPending || empty.isPending

  // Toasted rather than shown above the list: a message that appears in the
  // flow pushes every row down the moment one of them fails, which is the same
  // shift the command band exists to prevent.
  const failed = (what: string) => (err: unknown) =>
    void toast.error((err as Error)?.message ?? `Could not ${what}`)

  /**
   * A bulk call answers per id, and a 200 is not the same as "it worked". The
   * rows that could not go back keep their place in the list and are named —
   * a silent no-op on three of twenty is the failure mode worth spending a
   * toast on.
   */
  const report = (verb: string) => (outcome: BulkOutcome) => {
    if (outcome.conflicts.length > 0)
      toast.error(`${name(outcome.conflicts)} stayed in the trash — something with that name is already there.`)
    if (outcome.failed.length > 0) toast.error(`Could not ${verb} ${name(outcome.failed)}.`)
    if (outcome.stalled) toast.error('Some of the trash is still there. Try again.')
  }

  const onRestore = (nodes: readonly DriveNode[]) =>
    restore.mutate(nodes, { onSuccess: report('restore'), onError: failed('restore that') })

  const onPurge = (nodes: readonly DriveNode[]) =>
    purge.mutate(nodes, { onSuccess: report('delete'), onError: failed('delete that') })

  const bandActions = (chosen: readonly DriveNode[]): BandAction[] => [
    { label: 'Restore', icon: ArchiveRestore, disabled: busy, onSelect: () => onRestore(chosen) },
    { label: 'Delete forever', icon: Trash2, disabled: busy, danger: true, onSelect: () => onPurge(chosen) },
  ]

  /**
   * Not `rowActions` from the row menu: nothing in the trash can be renamed,
   * copied or moved while it is here, and the two things it can do are not on
   * that list at all.
   *
   * Neither reaches the list's Delete key, which is why `onDelete` is not
   * passed at all. In a folder that key moves rows to the trash and the trash
   * is where they can be got back from; here the same key would destroy them,
   * with no dialog in front of it and nothing behind it. Deleting for good is
   * worth reaching for on purpose — the band, or this menu.
   */
  const rowActions = (node: DriveNode): Action[] => [
    { label: 'Restore', icon: ArchiveRestore, disabled: busy, onSelect: () => onRestore([node]) },
    {
      label: 'Delete forever',
      icon: Trash2,
      disabled: busy,
      danger: true,
      separatorBefore: true,
      onSelect: () => onPurge([node]),
    },
  ]

  return (
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <div>
        <h1 className="text-[17px] font-semibold tracking-tight">Trash</h1>
        <p className="text-[13px] text-ink-3">
          Restoring puts a node back where it came from. If something already has its name there, it comes back
          renamed.
        </p>
      </div>
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
        selectable
        bandActions={bandActions}
        bandIdle={
          items.length > 0 && (
            <Button
              variant="ghost"
              size="sm"
              className="shrink-0 hover:bg-danger-soft hover:text-danger"
              disabled={busy}
              onClick={() => setConfirming(true)}
            >
              <Trash2 />
              Empty trash
            </Button>
          )
        }
        actions={rowActions}
        linkNames={false}
        time={{ label: 'Trashed', of: (node) => node.deleted_at }}
        more={{
          has: trash.hasNextPage,
          loading: trash.isFetchingNextPage,
          load: () => void trash.fetchNextPage(),
        }}
      />

      {confirming && (
        <EmptyTrashDialog
          count={items.length}
          // Only the whole trash can be counted out loud, and the whole trash
          // is loaded exactly when there is no page left to fetch.
          exact={!trash.hasNextPage}
          busy={busy}
          onCancel={() => setConfirming(false)}
          onConfirm={() => {
            setConfirming(false)
            empty.mutate(undefined, {
              onSuccess: ({ stalled }) => {
                if (stalled) toast.error('Some of the trash is still there. Try again.')
              },
              onError: failed('empty the trash'),
            })
          }}
        />
      )}
    </main>
  )
}

/** Names the rows a call could not finish with, without listing twenty of them. */
function name(nodes: DriveNode[]): string {
  const shown = nodes
    .slice(0, 3)
    .map((node) => `“${node.name}”`)
    .join(', ')
  return nodes.length > 3 ? `${shown} and ${nodes.length - 3} more` : shown
}
