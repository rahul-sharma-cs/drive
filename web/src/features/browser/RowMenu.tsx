import { Copy, Download, Eye, FolderInput, MoreVertical, Pencil, Trash2, type LucideIcon } from 'lucide-react'
import { useRef, type ReactNode } from 'react'
import { Link, useSearchParams } from 'react-router'

import { Button } from '@/components/ui/button'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

import { downloadHref, type DriveNode } from '../../lib/api'
import { previewTarget } from '../preview/usePreview'

/**
 * One row's commands, described once and rendered twice — by the kebab at the
 * end of the row and by the right-click menu around the whole row. Two menus
 * that offer different things on the same row is the bug this shape exists to
 * make impossible.
 *
 * Both menus are `modal={false}`: Radix's modal mode puts
 * `pointer-events: none` on the body while a menu is open, and this app already
 * regressed once on that surviving an unmount. A row menu that opens a dialog
 * as it closes is exactly that case, and the row underneath is a drop target
 * the whole time the menu is open.
 */

export interface Action {
  /** The menu label, and the item's key. */
  label: string
  icon: LucideIcon
  /** A command. Mutually exclusive with `href`. */
  onSelect?: () => void
  /** A navigation — Download is a link, never a fetch. */
  href?: string
  /**
   * Opens this file in the viewer, on the route already on screen. A link
   * rather than a command, like the name it duplicates, so a middle click
   * still opens it in its own tab.
   */
  previewId?: string
  disabled?: boolean
  /** Red, and last: the item that throws something away. */
  danger?: boolean
  /** Draws a rule above this item. */
  separatorBefore?: boolean
}

/** What a row's commands need from the screen holding the dialogs. */
export interface RowHandlers {
  onRename: (node: DriveNode) => void
  onCopy: (node: DriveNode) => void
  onMove: (node: DriveNode) => void
  onTrash: (node: DriveNode) => void
  /** A folder: zip it in the browser. Must run inside the click, not after it. */
  onZip: (node: DriveNode) => void
}

/**
 * The commands one node offers.
 *
 * Preview leads for a file, above Download: it is the same thing the name does,
 * and a menu that only offered to download what the row will happily show would
 * be hiding its own best answer. A folder has no preview — it opens by being
 * navigated into — but it does download, as a zip built in this tab.
 *
 * `extra` is the slot share links will arrive in — items are spliced in after
 * Move to, so the destructive one stays last wherever it is rendered.
 */
export function rowActions(node: DriveNode, handlers: RowHandlers, extra: Action[] = []): Action[] {
  const isFile = node.kind === 'file'
  return [
    ...(isFile ? [{ label: 'Preview', icon: Eye, previewId: node.id } satisfies Action] : []),
    isFile
      ? ({ label: 'Download', icon: Download, href: downloadHref(node.id) } satisfies Action)
      : ({ label: 'Download', icon: Download, onSelect: () => handlers.onZip(node) } satisfies Action),
    { label: 'Rename', icon: Pencil, onSelect: () => handlers.onRename(node) },
    ...(isFile
      ? [{ label: 'Make a copy', icon: Copy, onSelect: () => handlers.onCopy(node) } satisfies Action]
      : []),
    { label: 'Move to', icon: FolderInput, onSelect: () => handlers.onMove(node) },
    ...extra,
    {
      label: 'Move to trash',
      icon: Trash2,
      onSelect: () => handlers.onTrash(node),
      danger: true,
      separatorBefore: true,
    },
  ]
}

/** The kebab at the end of a row. */
export function RowMenu({ actions, label }: { actions: Action[]; label: string }) {
  return (
    <DropdownMenu modal={false}>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={label}
          // The row is the drag handle; a button inside it must not become one.
          draggable={false}
          onDragStart={(e) => e.preventDefault()}
        >
          <MoreVertical />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" sideOffset={4} className="w-52 rounded-pop p-1.5 shadow-pop">
        {actions.map((action) => (
          <MenuItem key={action.label} action={action} kind="dropdown" />
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/**
 * How long after a menu opens its items refuse a `pointerup`. The menu grows
 * into place over 150ms (`animate-in zoom-in-95`), and it is only while it is
 * growing that an item can be under a cursor the menu was summoned from — so
 * anything past the animation is a real click on a settled menu.
 */
const OPENING_MS = 200

/**
 * The same commands on right-click. The trigger wraps the row itself rather
 * than sitting inside it, so the whole row answers — including the empty space
 * between the name and the size.
 */
export function RowContextMenu({ actions, children }: { actions: Action[]; children: ReactNode }) {
  const openedAt = useRef(0)

  return (
    <ContextMenu
      modal={false}
      onOpenChange={(open) => {
        if (open) openedAt.current = performance.now()
      }}
    >
      <ContextMenuTrigger asChild>{children}</ContextMenuTrigger>
      <ContextMenuContent
        // The menu is 208px wide, is anchored at the cursor, and can only be
        // put to the left or the right of it — so on a phone a right-click near
        // the middle of the screen has room for it on neither side and it hangs
        // off the edge. Capping it at the width Radix measured as free on the
        // side it chose is what keeps it on screen; the padding leaves a margin
        // rather than ending it flush against the edge.
        collisionPadding={8}
        className="w-52 max-w-(--radix-context-menu-content-available-width) rounded-pop p-1.5 shadow-pop"
        // A right-click quick enough that the button comes back up while the
        // menu is still growing drops its `pointerup` on whichever item has
        // just spread under the cursor — and an item Radix never saw a
        // `pointerdown` on selects on `pointerup`. Near the bottom of the
        // screen the menu opens upward and that item is "Move to trash": a
        // click that asked for a menu runs a command instead. Swallowing the
        // event for the length of the animation costs nothing — a real click
        // on an item arrives as a `click`, and the keyboard never comes
        // through here at all.
        onPointerUpCapture={(event) => {
          if (performance.now() - openedAt.current < OPENING_MS) event.stopPropagation()
        }}
      >
        {actions.map((action) => (
          <MenuItem key={action.label} action={action} kind="context" />
        ))}
      </ContextMenuContent>
    </ContextMenu>
  )
}

function MenuItem({ action, kind }: { action: Action; kind: 'dropdown' | 'context' }) {
  const Item = kind === 'dropdown' ? DropdownMenuItem : ContextMenuItem
  const Separator = kind === 'dropdown' ? DropdownMenuSeparator : ContextMenuSeparator
  // The viewer rides the current query string, so an item that opens it has to
  // be built from what is already there rather than from the id alone.
  const [params] = useSearchParams()
  const Icon = action.icon
  const body = (
    <>
      <Icon />
      {action.label}
    </>
  )

  return (
    <>
      {action.separatorBefore && <Separator />}
      <Item
        variant={action.danger ? 'destructive' : 'default'}
        disabled={action.disabled}
        onSelect={action.onSelect}
        asChild={action.href !== undefined || action.previewId !== undefined}
      >
        {action.previewId !== undefined ? (
          <Link to={previewTarget(action.previewId, params)} draggable={false}>
            {body}
          </Link>
        ) : action.href === undefined ? (
          body
        ) : (
          // A download is a navigation to the 302 the API answers with, in its
          // own tab: the bytes come from the object store and must never pass
          // through this app, and a 401 rendered in this tab would replace it.
          <a href={action.href} target="_blank" rel="noopener" draggable={false}>
            {body}
          </a>
        )}
      </Item>
    </>
  )
}
