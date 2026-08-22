import { Fragment, useState } from 'react'
import { Link, useParams } from 'react-router'
import { toast } from 'sonner'

import { downloadHref, type DriveNode } from '../../lib/api'
import { Card, EmptyState, ghostButtonClass, secondaryButtonClass, SkeletonRows } from '../../ui/controls'
import { DownloadIcon, FileIcon, FolderIcon, TrashIcon, UploadIcon } from '../../ui/icons'
import { formatBytes } from '../../ui/format'
import { formatWhen } from '../../ui/when'
import { useSession } from '../auth/session'
import { DropZone } from '../upload/ui/DropZone'
import { DestinationDialog } from './DestinationDialog'
import { NewFolderDialog } from './NewFolderDialog'
import { RenameDialog } from './RenameDialog'
import { SelectionToolbar } from './SelectionToolbar'
import { useBreadcrumbs, useChildren, useCopyNode, useTrashNode, useUpdateNode } from './queries'

/** The MIME type an internal drag carries. Files dragged in carry `Files`. */
export const NODE_DRAG_TYPE = 'application/x-drive-node'

export function FolderPage() {
  const { id } = useParams()
  const session = useSession()
  const folderId = id ?? session.root_id

  return <FolderView key={folderId} folderId={folderId} rootId={session.root_id} />
}

type Dialog = { kind: 'rename'; node: DriveNode } | { kind: 'move' } | { kind: 'copy' } | null

/**
 * Keyed on the folder id by its parent, so navigating between folders remounts
 * rather than showing the previous folder's rows under the new breadcrumbs —
 * which also drops the selection, since a selection made in one folder means
 * nothing in the next.
 */
function FolderView({ folderId, rootId }: { folderId: string; rootId: string }) {
  const children = useChildren(folderId)
  const crumbs = useBreadcrumbs(folderId)
  const trash = useTrashNode(folderId)
  const update = useUpdateNode(folderId)
  const copy = useCopyNode(folderId)

  const [selected, setSelected] = useState<string[]>([])
  const [dialog, setDialog] = useState<Dialog>(null)
  const [dropTarget, setDropTarget] = useState<string | null>(null)

  const nodes = children.data?.pages.flatMap((page) => page.items) ?? []
  const chosen = nodes.filter((n) => selected.includes(n.id))

  const toggle = (id: string, additive: boolean) =>
    setSelected((was) =>
      additive ? (was.includes(id) ? was.filter((x) => x !== id) : [...was, id]) : was.includes(id) && was.length === 1 ? [] : [id],
    )

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

  const moveTo = (destination: string, ids: string[] = selected) =>
    withConflictRetry(
      (policy) =>
        Promise.all(ids.map((id) => update.mutateAsync({ id, parent_id: destination, conflict_policy: policy }))),
      'move that',
    ).then(() => {
      setSelected([])
      setDialog(null)
    })

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <div className="flex flex-wrap items-center gap-3">
        <Breadcrumbs
          crumbs={crumbs.data ?? []}
          rootId={rootId}
          dropTarget={dropTarget}
          setDropTarget={setDropTarget}
          onDropInto={(id) => void moveTo(id, dragPayload(null) ?? selected)}
        />
        <div className="ml-auto">
          <NewFolderDialog parentId={folderId} />
        </div>
      </div>

      <DropZone folderId={folderId}>
        <Card>
          {chosen.length > 0 && (
            <SelectionToolbar
              chosen={chosen}
              busy={update.isPending || copy.isPending || trash.isPending}
              onClear={() => setSelected([])}
              onRename={() => setDialog({ kind: 'rename', node: chosen[0] })}
              onMove={() => setDialog({ kind: 'move' })}
              onCopy={() => setDialog({ kind: 'copy' })}
              onDelete={() => {
                chosen.forEach((n) => trash.mutate(n.id))
                setSelected([])
              }}
            />
          )}

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
            <>
              <ColumnHeader />
              <ul className="divide-y divide-line">
                {nodes.map((node) => (
                  <Row
                    key={node.id}
                    node={node}
                    selected={selected.includes(node.id)}
                    dropTarget={dropTarget === node.id}
                    onToggle={toggle}
                    onDragStartRow={() => {
                      // Dragging an unselected row acts on that row alone,
                      // which is what every file manager does and what stops a
                      // stale selection from moving with it.
                      if (!selected.includes(node.id)) setSelected([node.id])
                    }}
                    onDropInto={(ids) => void moveTo(node.id, ids)}
                    setDropTarget={setDropTarget}
                    onTrash={() => trash.mutate(node.id)}
                    busy={trash.isPending}
                  />
                ))}
              </ul>
            </>
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
            ).then(() => {
              setSelected([])
              setDialog(null)
            })
          }
        />
      )}

      {dialog?.kind === 'move' && (
        <DestinationDialog
          title={`Move ${describe(chosen)}`}
          action="Move here"
          rootId={rootId}
          excludeIds={selected}
          busy={update.isPending}
          onCancel={() => setDialog(null)}
          onPick={(destination) => void moveTo(destination)}
        />
      )}

      {dialog?.kind === 'copy' && (
        <DestinationDialog
          title={`Copy ${describe(chosen)}`}
          action="Copy here"
          rootId={rootId}
          excludeIds={[]}
          busy={copy.isPending}
          onCancel={() => setDialog(null)}
          onPick={(destination) =>
            void withConflictRetry(
              (policy) =>
                Promise.all(
                  chosen
                    .filter((n) => n.kind === 'file')
                    .map((n) => copy.mutateAsync({ id: n.id, destination, conflictPolicy: policy })),
                ),
              'copy that',
            ).then(() => {
              setSelected([])
              setDialog(null)
            })
          }
        />
      )}
    </main>
  )
}

function describe(chosen: DriveNode[]): string {
  return chosen.length === 1 ? `“${chosen[0].name}”` : `${chosen.length} items`
}

/** The ids riding on an internal drag, or null if this is not one of ours. */
function dragPayload(dt: DataTransfer | null): string[] | null {
  const raw = dt?.getData(NODE_DRAG_TYPE)
  if (!raw) return null
  try {
    const ids = JSON.parse(raw)
    return Array.isArray(ids) && ids.length > 0 ? (ids as string[]) : null
  } catch {
    return null
  }
}

/** True when a dragover carries our own payload — readable by type only. */
function isNodeDrag(dt: DataTransfer | null): boolean {
  return dt !== null && Array.from(dt.types).includes(NODE_DRAG_TYPE)
}

function ColumnHeader() {
  return (
    <div className="flex items-center gap-3 border-b border-line bg-surface-muted px-3 py-2 text-[12px] font-medium text-ink-3 sm:px-4">
      <span className="w-4 shrink-0" aria-hidden />
      <span className="w-4 shrink-0" aria-hidden />
      <span className="min-w-0 flex-1">Name</span>
      <span className="hidden w-32 shrink-0 sm:block">Modified</span>
      <span className="w-16 shrink-0 text-right">Size</span>
      <span className="hidden shrink-0 sm:block sm:w-[6.5rem]" aria-hidden />
    </div>
  )
}

function Breadcrumbs({
  crumbs,
  rootId,
  dropTarget,
  setDropTarget,
  onDropInto,
}: {
  crumbs: DriveNode[]
  rootId: string
  dropTarget: string | null
  setDropTarget: (id: string | null) => void
  onDropInto: (id: string) => void
}) {
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
              // An ancestor is a drop target too — moving something "up" is
              // otherwise the one direction dragging cannot express.
              <Link
                className={`truncate rounded px-1 transition duration-100 hover:text-ink ${
                  dropTarget === crumb.id ? 'bg-teal-soft text-teal-strong ring-1 ring-teal' : ''
                }`}
                to={crumb.id === rootId ? '/' : `/folders/${crumb.id}`}
                onDragOver={(e) => {
                  if (!isNodeDrag(e.dataTransfer)) return
                  e.preventDefault()
                  setDropTarget(crumb.id)
                }}
                onDragLeave={() => setDropTarget(null)}
                onDrop={(e) => {
                  if (!isNodeDrag(e.dataTransfer)) return
                  e.preventDefault()
                  setDropTarget(null)
                  onDropInto(crumb.id)
                }}
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

function Row({
  node,
  selected,
  dropTarget,
  onToggle,
  onDragStartRow,
  onDropInto,
  setDropTarget,
  onTrash,
  busy,
}: {
  node: DriveNode
  selected: boolean
  dropTarget: boolean
  onToggle: (id: string, additive: boolean) => void
  onDragStartRow: () => void
  onDropInto: (ids: string[]) => void
  setDropTarget: (id: string | null) => void
  onTrash: () => void
  busy: boolean
}) {
  const isFolder = node.kind === 'folder'

  return (
    <li
      // The row is the drag handle. The name is a link, and a link drags itself
      // by default carrying its own URL — which is what used to hand the drop
      // zone a "file" that was really a hyperlink.
      draggable
      onDragStart={(e) => {
        onDragStartRow()
        const ids = selected ? undefined : [node.id]
        e.dataTransfer.effectAllowed = 'move'
        e.dataTransfer.setData(NODE_DRAG_TYPE, JSON.stringify(ids ?? [node.id]))
      }}
      onDragOver={
        isFolder
          ? (e) => {
              if (!isNodeDrag(e.dataTransfer)) return
              e.preventDefault()
              e.dataTransfer.dropEffect = 'move'
              setDropTarget(node.id)
            }
          : undefined
      }
      onDragLeave={isFolder ? () => setDropTarget(null) : undefined}
      onDrop={
        isFolder
          ? (e) => {
              if (!isNodeDrag(e.dataTransfer)) return
              e.preventDefault()
              setDropTarget(null)
              const ids = JSON.parse(e.dataTransfer.getData(NODE_DRAG_TYPE) || '[]') as string[]
              if (ids.length > 0 && !ids.includes(node.id)) onDropInto(ids)
            }
          : undefined
      }
      onClick={(e) => {
        // A click on a link or a control is that control's, not the row's.
        if ((e.target as HTMLElement).closest('a,button,input')) return
        onToggle(node.id, e.metaKey || e.ctrlKey)
      }}
      className={`group flex items-center gap-3 px-3 py-2.5 transition duration-100 sm:px-4 ${
        dropTarget
          ? 'bg-teal-soft ring-1 ring-inset ring-teal'
          : selected
            ? 'bg-teal-soft/60'
            : 'hover:bg-surface-muted'
      }`}
    >
      <input
        type="checkbox"
        className={`h-4 w-4 shrink-0 accent-teal transition-opacity duration-100 ${
          selected
            ? 'opacity-100'
            : 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100 [@media(pointer:coarse)]:opacity-100'
        }`}
        checked={selected}
        aria-label={`Select ${node.name}`}
        onChange={() => onToggle(node.id, true)}
      />
      <span className={isFolder ? 'shrink-0 text-teal' : 'shrink-0 text-ink-3'}>
        {isFolder ? <FolderIcon /> : <FileIcon />}
      </span>
      {isFolder ? (
        <Link
          draggable={false}
          className="min-w-0 flex-1 truncate text-sm font-medium text-ink hover:underline"
          to={`/folders/${node.id}`}
        >
          {node.name}
        </Link>
      ) : (
        <span className="min-w-0 flex-1 truncate text-sm text-ink">{node.name}</span>
      )}
      <span className="numeric hidden w-32 shrink-0 text-ink-3 sm:block" title={new Date(node.updated_at).toString()}>
        {formatWhen(node.updated_at)}
      </span>
      <span className="numeric w-16 shrink-0 text-right text-ink-3">
        {node.size === null ? '' : formatBytes(node.size)}
      </span>
      {/* A fixed action column, so Delete sits in the same place on a folder
          row (no Download) as on a file row. */}
      <div className="flex shrink-0 items-center justify-end gap-1 sm:w-[6.5rem]">
        {node.kind === 'file' && (
          // A plain navigation, not a fetch: the endpoint answers 302 to a
          // presigned URL and the bytes must never pass through this app. It
          // goes to its own tab because the download itself renders nothing
          // (the 302 carries `attachment`), while a 401 or 404 would otherwise
          // replace the whole app with a JSON error body.
          <a
            className={ghostButtonClass}
            href={downloadHref(node.id)}
            target="_blank"
            rel="noopener"
            draggable={false}
          >
            <DownloadIcon />
            {/* `sr-only`, not `hidden`: the word has to stay in the
                accessibility tree at every width, or the control loses its
                name on the screen that needs it most. */}
            <span className="sr-only">Download</span>
          </a>
        )}
        <button className={ghostButtonClass} onClick={onTrash} disabled={busy} aria-label={`Delete ${node.name}`}>
          <TrashIcon />
          <span className="sr-only">Delete</span>
        </button>
      </div>
    </li>
  )
}
