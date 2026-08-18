import { Fragment } from 'react'
import { Link, useParams } from 'react-router'

import { downloadHref, type DriveNode } from '../../lib/api'
import { Card, dangerButtonClass, EmptyState, ghostButtonClass, secondaryButtonClass, SkeletonRows } from '../../ui/controls'
import { DownloadIcon, FileIcon, FolderIcon, TrashIcon, UploadIcon } from '../../ui/icons'
import { formatBytes } from '../../ui/format'
import { useSession } from '../auth/session'
import { DropZone } from '../upload/ui/DropZone'
import { NewFolderDialog } from './NewFolderDialog'
import { useBreadcrumbs, useChildren, useTrashNode } from './queries'

export function FolderPage() {
  const { id } = useParams()
  const session = useSession()
  const folderId = id ?? session.root_id

  return <FolderView key={folderId} folderId={folderId} rootId={session.root_id} />
}

/**
 * Keyed on the folder id by its parent, so navigating between folders remounts
 * rather than showing the previous folder's rows under the new breadcrumbs.
 */
function FolderView({ folderId, rootId }: { folderId: string; rootId: string }) {
  const children = useChildren(folderId)
  const crumbs = useBreadcrumbs(folderId)
  const trash = useTrashNode(folderId)

  const nodes = children.data?.pages.flatMap((page) => page.items) ?? []

  return (
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <div className="flex flex-wrap items-center gap-3">
        <Breadcrumbs crumbs={crumbs.data ?? []} rootId={rootId} />
        <div className="ml-auto">
          <NewFolderDialog parentId={folderId} />
        </div>
      </div>

      <DropZone folderId={folderId}>
        <Card>
          {children.isPending && <SkeletonRows />}
          {children.error && (
            <div role="alert" className="flex flex-col items-start gap-2 px-4 py-6">
              <p className="text-sm text-ink-2">This folder didn’t load.</p>
              <button className={secondaryButtonClass} onClick={() => void children.refetch()}>
                Try again
              </button>
            </div>
          )}
          {children.isSuccess && nodes.length === 0 && (
            <EmptyState
              icon={<UploadIcon />}
              title="This folder is empty."
              hint="Drop files or folders here, or use the buttons above."
            />
          )}
          {nodes.length > 0 && (
            <ul className="divide-y divide-line">
              {nodes.map((node) => (
                <Row key={node.id} node={node} onTrash={() => trash.mutate(node.id)} busy={trash.isPending} />
              ))}
            </ul>
          )}
          {children.hasNextPage && (
            <div className="border-t border-line px-4 py-3">
              <button
                className={secondaryButtonClass}
                onClick={() => void children.fetchNextPage()}
                disabled={children.isFetchingNextPage}
              >
                {children.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            </div>
          )}
        </Card>
      </DropZone>
    </main>
  )
}

function Breadcrumbs({ crumbs, rootId }: { crumbs: DriveNode[]; rootId: string }) {
  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 flex-wrap items-center gap-1.5 text-sm text-ink-3">
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1
        const label = crumb.id === rootId ? 'My Drive' : crumb.name
        return (
          <Fragment key={crumb.id}>
            {i > 0 && (
              <span aria-hidden className="text-line-strong">
                /
              </span>
            )}
            {last ? (
              // The trail doubles as the page title: the folder you are in is
              // the largest thing on the screen after the files themselves.
              <span aria-current="page" className="truncate text-[17px] font-semibold tracking-tight text-ink">
                {label}
              </span>
            ) : (
              <Link
                className="truncate rounded px-0.5 transition duration-100 hover:text-ink"
                to={crumb.id === rootId ? '/' : `/folders/${crumb.id}`}
              >
                {label}
              </Link>
            )}
          </Fragment>
        )
      })}
    </nav>
  )
}

function Row({ node, onTrash, busy }: { node: DriveNode; onTrash: () => void; busy: boolean }) {
  const isFolder = node.kind === 'folder'
  return (
    <li className="flex items-center gap-3 px-3 py-2.5 transition duration-100 hover:bg-surface-muted sm:px-4">
      <span className={isFolder ? 'shrink-0 text-accent' : 'shrink-0 text-ink-3'}>
        {isFolder ? <FolderIcon /> : <FileIcon />}
      </span>
      {isFolder ? (
        <Link className="min-w-0 truncate text-sm font-medium text-ink hover:underline" to={`/folders/${node.id}`}>
          {node.name}
        </Link>
      ) : (
        <span className="min-w-0 truncate text-sm text-ink">{node.name}</span>
      )}
      <span className="numeric ml-auto w-14 shrink-0 text-right text-ink-3 sm:w-16">
        {node.size === null ? '' : formatBytes(node.size)}
      </span>
      {/* A fixed action column, so Delete sits in the same place on a folder
          row (no Download) as on a file row. */}
      <div className="flex shrink-0 items-center justify-end gap-1 sm:w-[10.5rem]">
        {node.kind === 'file' && (
          // A plain navigation, not a fetch: the endpoint answers 302 to a
          // presigned URL and the bytes must never pass through this app. It
          // goes to its own tab because the download itself renders nothing
          // (the 302 carries `attachment`), while a 401 or 404 would otherwise
          // replace the whole app with a JSON error body.
          <a className={ghostButtonClass} href={downloadHref(node.id)} target="_blank" rel="noopener">
            <DownloadIcon />
            {/* `sr-only`, not `hidden`: the word has to stay in the
                accessibility tree at every width, or the control loses its
                name on the screen that needs it most. */}
            <span className="sr-only sm:not-sr-only">Download</span>
          </a>
        )}
        <button className={dangerButtonClass} onClick={onTrash} disabled={busy}>
          <TrashIcon />
          <span className="sr-only sm:not-sr-only">Delete</span>
        </button>
      </div>
    </li>
  )
}
