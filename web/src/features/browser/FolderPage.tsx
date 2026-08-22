import { useParams } from 'react-router'

import { EmptyState } from '../../ui/controls'
import { UploadIcon } from '../../ui/icons'
import { useSession } from '../auth/session'
import { PreviewDialog } from '../preview/PreviewDialog'
import { DropZone } from '../upload/ui/DropZone'
import { Breadcrumbs } from './Breadcrumbs'
import { FileList } from './FileList'
import { rowActions } from './RowMenu'
import { nodeBandActions, useNodeCommands } from './commands'
import { useBreadcrumbs, useChildren } from './queries'

export function FolderPage() {
  const { id } = useParams()
  const session = useSession()
  const folderId = id ?? session.root_id

  return <FolderView key={folderId} folderId={folderId} rootId={session.root_id} />
}

/**
 * One folder: the trail, the list, and the commands they share.
 *
 * The list itself — selection, keys, row menus, the command band — is
 * `FileList`, which search and the trash render too, and the commands and their
 * dialogs are `useNodeCommands`. What is left here is what only this screen
 * knows: which folder is on screen, and that its rows can be dragged into each
 * other and up onto a crumb.
 *
 * Keyed on the folder id by its parent, so navigating between folders remounts
 * rather than showing the previous folder's rows under the new breadcrumbs —
 * which also drops the selection, since a selection made in one folder means
 * nothing in the next.
 */
function FolderView({ folderId, rootId }: { folderId: string; rootId: string }) {
  const children = useChildren(folderId)
  const crumbs = useBreadcrumbs(folderId)
  const { handlers, commands, busy, moveTo, dialogs } = useNodeCommands(folderId)

  const nodes = children.data?.pages.flatMap((page) => page.items) ?? []

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <Breadcrumbs crumbs={crumbs.data ?? []} rootId={rootId} onDropInto={(id, ids) => void moveTo(id, ids)} />

      <DropZone>
        <FileList
          nodes={nodes}
          pending={children.isPending}
          error={children.error}
          onRetry={() => void children.refetch()}
          empty={
            <EmptyState
              icon={<UploadIcon />}
              title="This folder is empty."
              hint="Drop files or folders here, or add them from the New menu."
            />
          }
          selectable
          bandActions={(chosen) => nodeBandActions(chosen, commands, busy)}
          onDelete={commands.onTrash}
          actions={(node) => rowActions(node, handlers)}
          dnd={{ onMoveInto: (destination, ids) => void moveTo(destination, ids) }}
          more={{
            has: children.hasNextPage,
            loading: children.isFetchingNextPage,
            load: () => void children.fetchNextPage(),
          }}
        />
      </DropZone>

      {/* Rendered here rather than inside the list: opening a file must not
          remount the rows behind it. */}
      <PreviewDialog nodes={nodes} hasMore={children.hasNextPage} />

      {dialogs}
    </main>
  )
}
