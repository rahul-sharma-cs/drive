import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'

import type { DriveNode } from '../../lib/api'
import { Card, SkeletonRows } from '../../ui/controls'
import { CommandBand, type BandCommands } from './CommandBand'
import { FileRow, openTarget } from './FileRow'
import type { Action } from './RowMenu'
import { useListKeys } from './useListKeys'
import { useSelection } from './useSelection'

/**
 * The list every screen shows its nodes through — a folder, a search result,
 * the trash.
 *
 * It owns the three things that have to agree with each other: the selection,
 * the keyboard, and the command band. The band is rendered here rather than by
 * the screen because it must be the card's immediate sibling and must never
 * unmount — that pairing is the fix for a toolbar that used to push the list
 * down the moment you selected a row.
 *
 * What the screens keep is what only they know: where the rows come from, what
 * a command does to the server, and which dialogs answer for it.
 */

/** Dragging rows into folder rows. Absent on search and in the trash. */
export interface ListDnd {
  onMoveInto: (destinationId: string, ids: string[]) => void
}

export interface FileListProps {
  /** Every loaded row, in list order; the screen flattens its pages. */
  nodes: readonly DriveNode[]
  /** The first page is in flight — skeleton rows stand in for the answer. */
  pending?: boolean
  /** The listing failed. Replaces the rows, with Try again when `onRetry` is given. */
  error?: unknown
  /** What that failure says. Screens name their own subject. */
  errorText?: string
  onRetry?: () => void
  /** What a successful but empty listing says. */
  empty?: ReactNode
  /**
   * Row selection, the select-all checkbox and the command band. Off in the
   * trash, which has no bulk operations yet.
   */
  selectable?: boolean
  /** The commands the band's selected layer offers. Required when `selectable`. */
  commands?: BandCommands
  /** A mutation is in flight: the commands that would start another are off. */
  busy?: boolean
  /** The kebab and right-click items for one row. No menus are rendered without it. */
  actions?: (node: DriveNode) => Action[]
  /** Extra controls at the end of a row — the trash's Restore / Delete forever. */
  rowExtra?: (node: DriveNode) => ReactNode
  /** Names open the folder / the file. False in the trash, where neither opens. */
  linkNames?: boolean
  dnd?: ListDnd
  /** The infinite-query tail. */
  more?: { has: boolean; loading: boolean; load: () => void }
}

export function FileList({
  nodes,
  pending = false,
  error,
  errorText = 'This folder didn’t load.',
  onRetry,
  empty,
  selectable = false,
  commands,
  busy = false,
  actions,
  rowExtra,
  linkNames = true,
  dnd,
  more,
}: FileListProps) {
  const navigate = useNavigate()
  const selection = useSelection(nodes)
  const [dropTarget, setDropTarget] = useState<string | null>(null)

  // Rows in list order, not selection order: a command reads top to bottom.
  const chosen = selectable ? nodes.filter((n) => selection.selected.has(n.id)) : []

  const keys = useListKeys({
    count: nodes.length,
    selected: selectable ? selection.selected : EMPTY,
    onOpen: (index) => {
      const node = nodes[index]
      if (!node) return
      const target = openTarget(node)
      // The same destination the name link carries, so Enter and a click on the
      // name can never mean two different things.
      if ('to' in target) void navigate(target.to)
      else window.open(target.href, '_blank', 'noopener')
    },
    onTrash: (ids) => commands?.onTrash(nodes.filter((n) => ids.includes(n.id))),
    onSelectAll: () => selectable && selection.selectAll(),
    onClear: selection.clear,
  })

  // The hook reports which row the arrows are on; moving the browser's focus
  // there is this component's job, since only it holds the row elements. It
  // only ever moves focus that is already inside the list, so a dialog or a
  // menu that took focus away keeps it.
  const listRef = useRef<HTMLUListElement>(null)
  const takeFocus = useCallback(() => listRef.current?.focus(), [])
  const rowRefs = useRef<(HTMLLIElement | null)[]>([])
  const focused = keys.focusedIndex
  useEffect(() => {
    if (focused < 0) return
    const list = listRef.current
    if (!list || !list.contains(document.activeElement)) return
    rowRefs.current[focused]?.focus()
  }, [focused])

  // A refetch that fails while rows are on screen must leave them where they
  // are: the query keeps its last good data, so what is on screen is still
  // true, and an error block inside the card would push every row down for a
  // failure that changed nothing. Say so once per failure instead.
  const hasRows = nodes.length > 0
  const announced = useRef<unknown>(undefined)
  useEffect(() => {
    if (error == null) {
      announced.current = undefined
      return
    }
    if (!hasRows || announced.current === error) return
    announced.current = error
    toast.error(errorText, onRetry && { action: { label: 'Try again', onClick: onRetry } })
  }, [error, hasRows, errorText, onRetry])

  const rowDnd = dnd && {
    begin: (node: DriveNode) => {
      // Dragging an unselected row acts on that row alone, which is what every
      // file manager does and what stops a stale selection from moving with it.
      if (!selection.isSelected(node.id)) {
        selection.click(node.id)
        return [node.id]
      }
      return nodes.filter((n) => selection.selected.has(n.id)).map((n) => n.id)
    },
    drop: (node: DriveNode, ids: string[]) => dnd.onMoveInto(node.id, ids),
    over: dropTarget,
    setOver: setDropTarget,
  }

  return (
    <div className="flex flex-col">
      {selectable && commands && (
        <CommandBand
          count={nodes.length}
          chosen={chosen}
          busy={busy}
          onClear={selection.clear}
          onReturnFocus={takeFocus}
          commands={commands}
        />
      )}

      <Card>
        {pending && <SkeletonRows />}
        {error != null && !hasRows && (
          <div role="alert" className="flex flex-col items-start gap-2 px-4 py-6">
            <p className="text-sm text-ink-2">{errorText}</p>
            {onRetry && (
              <Button variant="outline" size="sm" onClick={onRetry}>
                Try again
              </Button>
            )}
          </div>
        )}
        {!pending && error == null && nodes.length === 0 && empty}

        {nodes.length > 0 && (
          <>
            <ColumnHeader
              selectable={selectable}
              gutter={actions !== undefined}
              count={nodes.length}
              allSelected={selection.allSelected}
              someSelected={selection.someSelected}
              onToggleAll={() => (selection.allSelected ? selection.clear() : selection.selectAll())}
            />
            {/* One tab stop for the whole list, with the arrows moving inside
                it; the handler catches what bubbles up from the rows. */}
            <ul
              ref={listRef}
              data-testid="file-list"
              tabIndex={-1}
              onKeyDown={keys.onKeyDown}
              className="divide-y divide-line outline-none"
            >
              {nodes.map((node, index) => (
                <FileRow
                  key={node.id}
                  ref={(el) => {
                    rowRefs.current[index] = el
                  }}
                  node={node}
                  selected={selection.selected.has(node.id)}
                  selectable={selectable}
                  linkName={linkNames}
                  actions={actions?.(node)}
                  extra={rowExtra?.(node)}
                  dnd={rowDnd}
                  keyProps={keys.rowProps(index)}
                  onClick={(e) => {
                    if (!selectable) return
                    keys.setFocusedIndex(index)
                    selection.click(node.id, e)
                  }}
                  onToggle={() => selection.toggle(node.id)}
                />
              ))}
            </ul>
          </>
        )}

        {more?.has && (
          <div className="border-t border-line px-4 py-3">
            <Button variant="outline" size="sm" onClick={more.load} disabled={more.loading}>
              {more.loading ? 'Loading…' : 'Load more'}
            </Button>
          </div>
        )}
      </Card>
    </div>
  )
}

const EMPTY: ReadonlySet<string> = new Set()

function ColumnHeader({
  selectable,
  gutter,
  count,
  allSelected,
  someSelected,
  onToggleAll,
}: {
  selectable: boolean
  gutter: boolean
  count: number
  allSelected: boolean
  someSelected: boolean
  onToggleAll: () => void
}) {
  return (
    <div className="flex h-9 items-center gap-3 border-b border-line bg-surface-muted px-3 text-[12px] font-medium text-ink-3 sm:px-4">
      {selectable && (
        <Checkbox
          data-testid="select-all"
          // Mixed is a third state, not an unchecked box: it says "some of
          // these", and clicking it takes the whole page.
          checked={allSelected ? true : someSelected ? 'indeterminate' : false}
          onCheckedChange={onToggleAll}
          aria-label={`Select all ${count} loaded`}
        />
      )}
      <span className="w-[22px] shrink-0" aria-hidden />
      <span className="min-w-0 flex-1">Name</span>
      <span className="hidden w-32 shrink-0 sm:block">Modified</span>
      <span className="w-16 shrink-0 text-right">Size</span>
      {/* Keeps Size over the size column rather than over the kebab. */}
      {gutter && <span className="w-8 shrink-0" aria-hidden />}
    </div>
  )
}
