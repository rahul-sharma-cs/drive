import { Fragment } from 'react'
import { Link, useParams } from 'react-router'

import { downloadHref, type DriveNode } from '../../lib/api'
import { secondaryButtonClass } from '../../ui/controls'
import { formatBytes } from '../../ui/format'
import { useSession } from '../auth/session'
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
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-6 py-6">
      <div className="flex items-center gap-3">
        <Breadcrumbs crumbs={crumbs.data ?? []} rootId={rootId} />
        <div className="ml-auto">
          <NewFolderDialog parentId={folderId} />
        </div>
      </div>

      <section className="rounded-xl border border-neutral-200 bg-white">
        {children.isPending && <p className="p-4 text-sm text-neutral-500">Loading…</p>}
        {children.error && (
          <p role="alert" className="p-4 text-sm text-red-700">
            This folder could not be loaded. Reload to try again.
          </p>
        )}
        {children.isSuccess && nodes.length === 0 && (
          <p className="p-4 text-sm text-neutral-500">This folder is empty.</p>
        )}
        <ul>
          {nodes.map((node) => (
            <Row key={node.id} node={node} onTrash={() => trash.mutate(node.id)} busy={trash.isPending} />
          ))}
        </ul>
        {children.hasNextPage && (
          <button
            className={`m-4 ${secondaryButtonClass}`}
            onClick={() => void children.fetchNextPage()}
            disabled={children.isFetchingNextPage}
          >
            {children.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </button>
        )}
      </section>
    </main>
  )
}

function Breadcrumbs({ crumbs, rootId }: { crumbs: DriveNode[]; rootId: string }) {
  return (
    <nav aria-label="Breadcrumb" className="flex flex-wrap items-center gap-1 text-sm text-neutral-600">
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1
        const label = crumb.id === rootId ? 'My Drive' : crumb.name
        return (
          <Fragment key={crumb.id}>
            {i > 0 && <span aria-hidden>/</span>}
            {last ? (
              <span aria-current="page" className="font-medium text-neutral-900">
                {label}
              </span>
            ) : (
              <Link className="hover:underline" to={crumb.id === rootId ? '/' : `/folders/${crumb.id}`}>
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
  return (
    <li className="flex items-center gap-3 border-b border-neutral-100 px-4 py-2.5 last:border-b-0">
      {node.kind === 'folder' ? (
        <Link className="text-sm font-medium hover:underline" to={`/folders/${node.id}`}>
          {node.name}
        </Link>
      ) : (
        <span className="text-sm">{node.name}</span>
      )}
      <span className="ml-auto text-xs text-neutral-500">{node.size === null ? '' : formatBytes(node.size)}</span>
      {node.kind === 'file' && (
        // A plain navigation, not a fetch: the endpoint answers 302 to a
        // presigned URL and the bytes must never pass through this app.
        <a className={secondaryButtonClass} href={downloadHref(node.id)}>
          Download
        </a>
      )}
      <button className={secondaryButtonClass} onClick={onTrash} disabled={busy}>
        Delete
      </button>
    </li>
  )
}
