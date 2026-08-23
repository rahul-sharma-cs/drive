import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { Button } from '@/components/ui/button'

import { getNode, type DriveNode } from '../../lib/api'
import { FileIcon } from '../../ui/FileIcon'
import { useChildren } from './queries'

/**
 * "Where to?" — the folder picker behind Move and Copy.
 *
 * It navigates rather than showing a tree: a tree would need every level
 * loaded at once, and the children endpoint is paginated per folder. Walking
 * one folder at a time asks for exactly what is on screen.
 *
 * Folders being moved are hidden from the list so a node cannot be dropped
 * into itself. Moving a folder into its own descendant is refused by the
 * server (it walks the ancestry inside the move transaction); the message
 * comes back to the caller rather than being pre-computed here.
 */
export function DestinationDialog({
  title,
  action,
  rootId,
  excludeIds,
  onPick,
  onCancel,
  busy,
}: {
  title: string
  /** The verb on the confirm button: "Move here", "Copy here". */
  action: string
  rootId: string
  excludeIds: string[]
  onPick: (folderId: string) => void
  onCancel: () => void
  busy: boolean
}) {
  const [trail, setTrail] = useState<DriveNode[]>([])
  const here = trail.length > 0 ? trail[trail.length - 1].id : rootId
  const children = useChildren(here)

  const folders = (children.data?.pages.flatMap((p) => p.items) ?? []).filter(
    (n) => n.kind === 'folder' && !excludeIds.includes(n.id),
  )

  const open = async (folder: DriveNode) => {
    // The listing already holds the node, but re-reading keeps the trail's
    // entries identical in shape to the root's, which is fetched.
    setTrail([...trail, await getNode(folder.id)])
  }

  return (
    <Dialog.Root open onOpenChange={(next) => !next && onCancel()}>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-1/2 z-50 flex max-h-[70vh] w-[min(26rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col rounded-pop border border-line bg-surface shadow-pop">
          <div className="border-b border-line px-5 py-4">
            <Dialog.Title className="text-[15px] font-semibold">{title}</Dialog.Title>
            <Dialog.Description className="mt-1 text-[13px] text-ink-3">
              Pick the folder to put {excludeIds.length === 1 ? 'it' : 'them'} in.
            </Dialog.Description>
            <nav aria-label="Destination" className="mt-3 flex flex-wrap items-center gap-1 text-[13px]">
              <Button variant="ghost" size="sm" onClick={() => setTrail([])}>
                My Drive
              </Button>
              {trail.map((crumb, i) => (
                <span key={crumb.id} className="flex items-center gap-1">
                  <span aria-hidden className="text-line-strong">
                    /
                  </span>
                  <Button variant="ghost" size="sm" onClick={() => setTrail(trail.slice(0, i + 1))}>
                    {crumb.name}
                  </Button>
                </span>
              ))}
            </nav>
          </div>

          <ul className="min-h-24 flex-1 overflow-y-auto py-1">
            {children.isPending && <li className="px-5 py-3 text-[13px] text-ink-3">Loading…</li>}
            {children.isSuccess && folders.length === 0 && (
              <li className="px-5 py-3 text-[13px] text-ink-3">No folders in here — {action.toLowerCase()}.</li>
            )}
            {folders.map((folder) => (
              <li key={folder.id}>
                <button
                  className="flex w-full items-center gap-2.5 px-5 py-2 text-left text-sm transition duration-100 hover:bg-surface-muted"
                  onClick={() => void open(folder)}
                >
                  {/* The same amber folder every list draws — the picker is
                      showing the same folders, so it says so the same way. */}
                  <FileIcon kind="folder" name={folder.name} size={20} />
                  <span className="min-w-0 truncate">{folder.name}</span>
                </button>
              </li>
            ))}
          </ul>

          <div className="flex justify-end gap-2 border-t border-line px-5 py-4">
            <Button variant="outline" onClick={onCancel}>
              Cancel
            </Button>
            <Button disabled={busy} onClick={() => onPick(here)}>
              {action}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
