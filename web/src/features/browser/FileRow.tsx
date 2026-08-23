import type { MouseEvent as ReactMouseEvent, ReactNode, Ref } from 'react'
import { Link, useNavigate, useSearchParams, type To } from 'react-router'

import { Checkbox } from '@/components/ui/checkbox'

import type { DriveNode } from '../../lib/api'
import { isPlainClick, navigateWithTransition } from '../../lib/viewTransition'
import { FileIcon } from '../../ui/FileIcon'
import { formatBytes } from '../../ui/format'
import { formatWhen } from '../../ui/when'
import { previewTarget } from '../preview/usePreview'
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

/**
 * Where a row's name points: a folder to itself, a file to the viewer over
 * whatever route is already on screen.
 *
 * `params` is the query string that route is carrying, and the file case is
 * seeded from it rather than rebuilt — on a search screen the query is what the
 * list underneath is made of, and opening a file must not throw it away.
 */
export function openTarget(node: DriveNode, params: URLSearchParams): { to: To } {
  return node.kind === 'folder' ? { to: `/folders/${node.id}` } : { to: previewTarget(node.id, params) }
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
  /** The ISO timestamp the middle column shows — modified, or trashed. */
  when: string | null | undefined
  /** The kebab and right-click items. Without them the row renders no menus. */
  actions?: Action[]
  /** Controls at the end of the row — the trash's Restore / Delete forever. */
  extra?: ReactNode
  dnd?: RowDnd
  /** Roving `tabIndex` and the focus marker from `useListKeys`. */
  keyProps: RowKeyProps
  onClick: (event: ReactMouseEvent) => void
  /**
   * A right-click landed on this row, before its menu opens. The list uses it
   * to make the row the selection, the way every file manager does.
   */
  onContextMenuOpen?: () => void
  onToggle: () => void
  ref?: Ref<HTMLLIElement>
}

export function FileRow({
  node,
  selected,
  selectable,
  linkName,
  when,
  actions,
  extra,
  dnd,
  keyProps,
  onClick,
  onContextMenuOpen,
  onToggle,
  ref,
}: FileRowProps) {
  const isFolder = node.kind === 'folder'
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const target = openTarget(node, params)
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
      // The trigger below clones this element, and a cloned child's handler
      // runs before the trigger's own — so this is the row's chance to become
      // the selection before the menu that acts on it opens.
      onContextMenu={onContextMenuOpen}
      // Two durations, because the two things being animated answer different
      // questions. The tint says "this is selected", which is a state and can
      // afford 100ms; the ring says "let go here", which is feedback on a
      // pointer that is still moving and has to arrive under it.
      className={`group flex h-12 items-center gap-3 px-3 outline-none [transition:background-color_100ms_var(--ease-out),box-shadow_80ms_var(--ease-out)] sm:px-4 ${
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
        // One affordance, and it is a real link: it underlines on hover, a
        // middle click opens it in its own tab, and the viewer it opens is a
        // location rather than a mode. Clicking anywhere else on the row
        // selects instead.
        <Link
          draggable={false}
          // What the viewer hands focus back to when it closes.
          data-preview-id={isFolder ? undefined : node.id}
          className={`min-w-0 flex-1 truncate text-sm text-ink hover:underline ${isFolder ? 'font-medium' : ''}`}
          to={target.to}
          onClick={
            // Opening a folder replaces the whole list, so it crossfades.
            // Opening a file does not — the viewer arrives over rows that stay
            // where they are, and it has an animation of its own.
            isFolder
              ? (e) => {
                  if (!isPlainClick(e)) return
                  e.preventDefault()
                  navigateWithTransition(() => void navigate(target.to))
                }
              : undefined
          }
        >
          {node.name}
        </Link>
      ) : (
        <span className="min-w-0 flex-1 truncate text-sm text-ink">{node.name}</span>
      )}

      {/* Not `.numeric`. "3 days ago" is a phrase, not a quantity — set in the
          mono face it reads as terminal output, and there are no columns of
          digits here to keep aligned. The size cell next to it is a quantity
          and keeps its tabular figures. */}
      <span
        className="hidden w-32 shrink-0 text-[12px] text-ink-3 sm:block"
        title={when ? new Date(when).toString() : undefined}
      >
        {when ? formatWhen(when) : ''}
      </span>
      <span className="numeric w-20 shrink-0 text-right text-ink-3">{meta(node)}</span>

      <div className="flex shrink-0 items-center justify-end gap-1">
        {extra}
        {actions && <RowMenu actions={actions} label={`Actions for ${node.name}`} />}
      </div>
    </li>
  )

  return actions ? <RowContextMenu actions={actions}>{row}</RowContextMenu> : row
}

/**
 * The trailing cell: how big a file is, or how much a folder holds.
 *
 * A folder has no size of its own worth quoting — summing a subtree is a walk
 * the listing does not do — so the useful quantity is the count, which the
 * children endpoint sends along. Listings that do not send it (search results,
 * the trash) leave the cell blank rather than claiming a folder is empty.
 */
function meta(node: DriveNode): string {
  if (node.kind === 'folder') {
    if (node.item_count == null) return ''
    return `${node.item_count} ${node.item_count === 1 ? 'item' : 'items'}`
  }
  return node.size === null ? '' : formatBytes(node.size)
}
