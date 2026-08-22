import { Copy, Download, FolderInput, Pencil, Trash2 } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { toast } from 'sonner'

import { downloadHref, type DriveNode } from '../../lib/api'
import { useSession } from '../auth/session'
import type { BandAction } from './CommandBand'
import { DestinationDialog } from './DestinationDialog'
import { RenameDialog } from './RenameDialog'
import type { RowHandlers } from './RowMenu'
import { useCopyNode, useTrashNode, useUpdateNode } from './queries'

/**
 * Rename, move, copy and trash — the four commands every list offers, the
 * dialogs that answer for them, and the conflict retry they share.
 *
 * A folder screen and a search screen ask for exactly the same things and
 * differ only in which listings a mutation invalidates, so the wiring lives
 * here once instead of being written out on each screen. The row menu acts on
 * the row it was opened from; the band acts on the selection; both end in the
 * same four places.
 *
 * `parentId` is the folder on screen, or omitted on search results, which span
 * every folder — see `childrenScope` in `queries.ts`.
 */

type Dialog =
  | { kind: 'rename'; node: DriveNode }
  | { kind: 'move'; nodes: DriveNode[] }
  | { kind: 'copy'; nodes: DriveNode[] }
  | null

/** What the band's commands need from the screen holding the dialogs. */
export interface BandCommands {
  onRename: (node: DriveNode) => void
  onCopy: (nodes: DriveNode[]) => void
  onMove: (nodes: DriveNode[]) => void
  onTrash: (nodes: DriveNode[]) => void
}

/**
 * What the band offers for a selection of live nodes — the folder screens and
 * search. The trash builds its own two.
 */
export function nodeBandActions(
  chosen: readonly DriveNode[],
  commands: BandCommands,
  busy: boolean,
): BandAction[] {
  const single = chosen.length === 1 ? chosen[0] : null
  const files = chosen.filter((n) => n.kind === 'file')

  return [
    // One file at a time: there is no archive endpoint to answer a
    // multi-selection with, and a button that silently downloaded one of five
    // would be a lie. It takes the whole selection once downloading a set of
    // files as one zip exists.
    ...(single?.kind === 'file'
      ? [{ label: 'Download', icon: Download, href: downloadHref(single.id) } satisfies BandAction]
      : []),
    ...(single
      ? [{ label: 'Rename', icon: Pencil, disabled: busy, onSelect: () => commands.onRename(single) } satisfies BandAction]
      : []),
    { label: 'Move to', icon: FolderInput, disabled: busy, onSelect: () => commands.onMove([...chosen]) },
    // Files only: the server answers a folder copy with 422.
    ...(files.length > 0
      ? [{ label: 'Copy to', icon: Copy, disabled: busy, onSelect: () => commands.onCopy(files) } satisfies BandAction]
      : []),
    { label: 'Trash', icon: Trash2, disabled: busy, danger: true, onSelect: () => commands.onTrash([...chosen]) },
  ]
}

export interface NodeCommands {
  /** For `rowActions`: each acts on the one row its menu was opened from. */
  handlers: RowHandlers
  /** For the command band: each acts on the selection. */
  commands: BandCommands
  /** A mutation is in flight — commands that would start another are off. */
  busy: boolean
  /** Move `ids` into `destination`. What a drop onto a folder or a crumb lands in. */
  moveTo: (destination: string, ids: string[]) => Promise<void>
  /** The dialogs the commands open. The screen renders this somewhere. */
  dialogs: ReactNode
}

export function useNodeCommands(parentId?: string): NodeCommands {
  const rootId = useSession().root_id
  const trash = useTrashNode(parentId)
  const update = useUpdateNode(parentId)
  const copy = useCopyNode(parentId)

  const [dialog, setDialog] = useState<Dialog>(null)

  /**
   * A collision is answered by the person, not decided for them: the first
   * attempt carries no policy, so the server reports the conflict, and the
   * offer to keep both is a retry with the same vocabulary the upload manager
   * uses.
   */
  const withConflictRetry = (run: (policy?: 'rename') => Promise<unknown>, what: string) =>
    run().catch((err: unknown) => {
      const conflict = (err as { code?: string })?.code === 'name_conflict'
      toast.error((err as Error)?.message ?? `Could not ${what}`, {
        action: conflict ? { label: 'Keep both', onClick: () => void run('rename') } : undefined,
      })
    })

  const moveTo = (destination: string, ids: string[]) =>
    withConflictRetry(
      (policy) =>
        Promise.all(ids.map((id) => update.mutateAsync({ id, parent_id: destination, conflict_policy: policy }))),
      'move that',
    ).then(() => setDialog(null))

  const handlers: RowHandlers = {
    onRename: (node) => setDialog({ kind: 'rename', node }),
    onMove: (node) => setDialog({ kind: 'move', nodes: [node] }),
    onCopy: (node) => setDialog({ kind: 'copy', nodes: [node] }),
    onTrash: (node) => trash.mutate(node.id),
  }

  const commands: BandCommands = {
    onRename: handlers.onRename,
    onMove: (nodes) => setDialog({ kind: 'move', nodes }),
    onCopy: (nodes) => setDialog({ kind: 'copy', nodes }),
    onTrash: (nodes) => nodes.forEach((n) => trash.mutate(n.id)),
  }

  const dialogs = (
    <>
      {dialog?.kind === 'rename' && (
        <RenameDialog
          currentName={dialog.node.name}
          error={update.error}
          busy={update.isPending}
          onCancel={() => {
            update.reset()
            setDialog(null)
          }}
          onRename={(name) =>
            void withConflictRetry(
              (policy) => update.mutateAsync({ id: dialog.node.id, name, conflict_policy: policy }),
              'rename that',
            ).then(() => setDialog(null))
          }
        />
      )}

      {dialog?.kind === 'move' && (
        <DestinationDialog
          title={`Move ${describe(dialog.nodes)}`}
          action="Move here"
          rootId={rootId}
          // A folder cannot be moved inside itself, and neither can anything
          // else that is on its way somewhere.
          excludeIds={dialog.nodes.map((n) => n.id)}
          busy={update.isPending}
          onCancel={() => setDialog(null)}
          onPick={(destination) =>
            void moveTo(
              destination,
              dialog.nodes.map((n) => n.id),
            )
          }
        />
      )}

      {dialog?.kind === 'copy' && (
        <DestinationDialog
          title={`Copy ${describe(dialog.nodes)}`}
          action="Copy here"
          rootId={rootId}
          excludeIds={[]}
          busy={copy.isPending}
          onCancel={() => setDialog(null)}
          onPick={(destination) =>
            void withConflictRetry(
              // Files only: the server answers a folder copy with 422.
              (policy) =>
                Promise.all(
                  dialog.nodes
                    .filter((n) => n.kind === 'file')
                    .map((n) => copy.mutateAsync({ id: n.id, destination, conflictPolicy: policy })),
                ),
              'copy that',
            ).then(() => setDialog(null))
          }
        />
      )}
    </>
  )

  return {
    handlers,
    commands,
    busy: update.isPending || copy.isPending || trash.isPending,
    moveTo,
    dialogs,
  }
}

function describe(nodes: DriveNode[]): string {
  return nodes.length === 1 ? `“${nodes[0].name}”` : `${nodes.length} items`
}
