import type { MouseEvent as ReactMouseEvent, ReactNode, Ref } from 'react'
import { Link } from 'react-router'

import { Checkbox } from '@/components/ui/checkbox'

import { downloadHref, type DriveNode } from '../../lib/api'
import { FileIcon } from '../../ui/FileIcon'
import { formatBytes } from '../../ui/format'
import { formatWhen } from '../../ui/when'
import { dragPayload, isNodeDrag, setDragPayload } from './dnd'
import { RowContextMenu, RowMenu, type Action } from './RowMenu'
import type { RowKeyProps } from './useListKeys'

/**
 * One row: 48px tall, a hairline under it, a coloured type glyph leading.
 *
 * The name is the one thing on the row that opens it — it is a link, it
 * underlines on hover, and it is middle-clickable. Clicking anywhere else
 * selects, which is why the row's own click handler steps aside for anything
 * that is already a control.
 */

/** Where a row's name points. Phase C swaps the file case for `?preview=<id>`. */
export function openTarget(node: DriveNode): { to: string } | { href: string } {
  return node.kind === 'folder' ? { to: `/folders/${node.id}` } : { href: downloadHref(node.id) }
}

/** Dragging rows, as one row sees it. Absent on search and in the trash. */
export interface RowDnd {
  /** The drag is starting on this row; returns the ids it should carry. */
  begin: (node: DriveNode) => string[]
  /** This folder row was dropped on. */
  drop: (node: DriveNode, ids: string[]) => void
  /** Which folder row the pointer is over, so exactly one lights up. */
  over: string | null
  setOver: (id: string | null) => void
}

export interface FileRowProps {
  node: DriveNode
  selected: boolean
  selectable: boolean
  /** Names link to the folder / the file. False in the trash, where neither opens. */
  linkName: boolean
  /** The kebab and right-click items. Without them the row renders no menus. */
  actions?: Action[]
  /** Controls at the end of the row — the trash's Restore / Delete forever. */
  extra?: ReactNode
  dnd?: RowDnd
  /** Roving `tabIndex` and the focus marker from `useListKeys`. */
  keyProps: RowKeyProps
  onClick: (event: ReactMouseEvent) => void
  onToggle: () => void
  ref?: Ref<HTMLLIElement>
}

export function FileRow({
  node,
  selected,
  selectable,
  linkName,
  actions,
  extra,
  dnd,
  keyProps,
  onClick,
  onToggle,
  ref,
}: FileRowProps) {
  const isFolder = node.kind === 'folder'
  const target = openTarget(node)
  const dropTarget = dnd?.over === node.id

  const row = (
    <li
      ref={ref}
      data-testid="file-row"
      data-selected={selected ? '' : undefined}
      {...keyProps}
      // The row is the drag handle. A link drags itself by default carrying its
      // own URL, which is what used to hand the drop zone a "file" that was
      // really a hyperlink — hence `draggable={false}` on the name below.
      draggable={dnd !== undefined}
      onDragStart={
        dnd &&
        ((e) => {
          setDragPayload(e.dataTransfer, dnd.begin(node))
        })
      }
      onDragOver={
        dnd && isFolder
          ? (e) => {
              if (!isNodeDrag(e.dataTransfer)) return
              e.preventDefault()
              e.dataTransfer.dropEffect = 'move'
              dnd.setOver(node.id)
            }
          : undefined
      }
      onDragLeave={dnd && isFolder ? () => dnd.setOver(null) : undefined}
      onDrop={
        dnd && isFolder
          ? (e) => {
              if (!isNodeDrag(e.dataTransfer)) return
              e.preventDefault()
              dnd.setOver(null)
              const ids = dragPayload(e.dataTransfer) ?? []
              // A folder cannot be its own parent, and dropping a selection
              // that contains the target on the target is that same move.
              if (ids.length > 0 && !ids.includes(node.id)) dnd.drop(node, ids)
            }
          : undefined
      }
      onClick={(e) => {
        // A click on a link or a control is that control's, not the row's.
        if ((e.target as HTMLElement).closest('a,button,input,label')) return
        onClick(e)
      }}
      className={`group flex h-12 items-center gap-3 px-3 outline-none transition duration-100 sm:px-4 ${
        dropTarget
          ? 'bg-teal-soft ring-1 ring-inset ring-teal'
          : selected
            ? 'bg-teal-soft/60'
            : 'hover:bg-surface-muted'
      } data-[focused]:ring-1 data-[focused]:ring-inset data-[focused]:ring-teal/40`}
    >
      {selectable && (
        <Checkbox
          checked={selected}
          onCheckedChange={onToggle}
          aria-label={`Select ${node.name}`}
          // Quiet until it is useful: the box appears on hover, on focus, on a
          // touch screen, and whenever the row is actually selected.
          className={`shrink-0 transition-opacity duration-100 ${
            selected
              ? 'opacity-100'
              : 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100 [@media(pointer:coarse)]:opacity-100'
          }`}
        />
      )}

      <FileIcon kind={node.kind} name={node.name} mime={node.mime} size={22} />

      {linkName ? (
        isFolder ? (
          <Link
            draggable={false}
            className="min-w-0 flex-1 truncate text-sm font-medium text-ink hover:underline"
            to={(target as { to: string }).to}
          >
            {node.name}
          </Link>
        ) : (
          // A plain navigation, not a fetch: the endpoint answers 302 to a
          // presigned URL and the bytes must never pass through this app. Its
          // own tab, because the 302 carries `attachment` and renders nothing,
          // while a 401 would otherwise replace the whole app with a JSON body.
          <a
            draggable={false}
            className="min-w-0 flex-1 truncate text-sm text-ink hover:underline"
            href={(target as { href: string }).href}
            target="_blank"
            rel="noopener"
          >
            {node.name}
          </a>
        )
      ) : (
        <span className="min-w-0 flex-1 truncate text-sm text-ink">{node.name}</span>
      )}

      <span
        className="numeric hidden w-32 shrink-0 text-ink-3 sm:block"
        title={new Date(node.updated_at).toString()}
      >
        {formatWhen(node.updated_at)}
      </span>
      <span className="numeric w-16 shrink-0 text-right text-ink-3">
        {node.size === null ? '' : formatBytes(node.size)}
      </span>

      <div className="flex shrink-0 items-center justify-end gap-1">
        {extra}
        {actions && <RowMenu actions={actions} label={`Actions for ${node.name}`} />}
      </div>
    </li>
  )

  return actions ? <RowContextMenu actions={actions}>{row}</RowContextMenu> : row
}
