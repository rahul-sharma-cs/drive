import { Copy, Download, FolderInput, MoreVertical, Pencil, Trash2, type LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'

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
}

/**
 * The commands one node offers.
 *
 * No Preview item yet: until the preview endpoint lands it would be a Download
 * wearing a different word, and two labels for one behaviour is worse than one
 * missing feature. It belongs at the top of this list, above Download.
 *
 * `extra` is the slot share links will arrive in — items are spliced in after
 * Move to, so the destructive one stays last wherever it is rendered.
 */
export function rowActions(node: DriveNode, handlers: RowHandlers, extra: Action[] = []): Action[] {
  const isFile = node.kind === 'file'
  return [
    ...(isFile
      ? [{ label: 'Download', icon: Download, href: downloadHref(node.id) } satisfies Action]
      : []),
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
 * The same commands on right-click. The trigger wraps the row itself rather
 * than sitting inside it, so the whole row answers — including the empty space
 * between the name and the size.
 */
export function RowContextMenu({ actions, children }: { actions: Action[]; children: ReactNode }) {
  return (
    <ContextMenu modal={false}>
      <ContextMenuTrigger asChild>{children}</ContextMenuTrigger>
      <ContextMenuContent className="w-52 rounded-pop p-1.5 shadow-pop">
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
        asChild={action.href !== undefined}
      >
        {action.href === undefined ? (
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
