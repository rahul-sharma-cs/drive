/**
 * Row selection for a file list.
 *
 * The selection is a Set of node ids rather than an array of nodes: rows are
 * re-fetched constantly (a mutation invalidates the page and every node object
 * is replaced), so holding objects would pin stale copies. Ids survive that,
 * and the list already has the nodes.
 *
 * Two rules the callers depend on:
 *
 *  - **Rows that leave the list leave the selection.** A trashed or moved row
 *    is gone from the next page of children, and a selection that still counts
 *    it would offer actions on something that is not there. Pruning happens
 *    during render, so `count` is never briefly wrong, and it keeps the rows
 *    that *are* still present untouched.
 *  - **Every callback is stable.** Rows can be `memo`ised on their own
 *    `selected` boolean without every handler changing identity underneath
 *    them on each keystroke.
 */

import { useCallback, useMemo, useRef, useState } from 'react'

import type { DriveNode } from '../../lib/api'

const EMPTY: ReadonlySet<string> = new Set()

/**
 * The modifier keys a row click carries. A React `MouseEvent` satisfies this
 * structurally, so a row can pass its event straight through.
 */
export interface ClickModifiers {
  metaKey?: boolean
  ctrlKey?: boolean
  shiftKey?: boolean
}

export interface Selection {
  /**
   * The selected ids, in the order they were selected. Callers that need list
   * order (a toolbar acting on rows top to bottom) should filter `nodes`
   * instead of reading this in order.
   */
  selected: ReadonlySet<string>
  /** `selected.size`, so a caller does not have to reach through the Set. */
  count: number
  /** True when the list is non-empty and every row in it is selected. */
  allSelected: boolean
  /** True when some but not all rows are — the header checkbox's `indeterminate`. */
  someSelected: boolean
  /**
   * Reads the live selection. Stable across selection changes on purpose, so
   * it is safe inside event handlers; render from `selected` instead, or a
   * memoised row will never hear that its own membership changed.
   */
  isSelected: (id: string) => boolean
  /** Add or remove one id, leaving the shift-click anchor where it is. */
  toggle: (id: string) => void
  selectAll: () => void
  clear: () => void
  /**
   * A click on the row body: plain selects only that row, cmd/ctrl toggles it,
   * shift selects the span from the anchor (the last plain or cmd click) to
   * this row inclusive. Shift does not move the anchor, so a second shift-click
   * re-ranges from the same place instead of walking the anchor along.
   */
  click: (id: string, mods?: ClickModifiers) => void
}

export function useSelection(nodes: readonly DriveNode[]): Selection {
  const [selected, setSelected] = useState<ReadonlySet<string>>(EMPTY)

  // The anchor is an id, not an index: rows move (a page loads above, Phase B
  // re-sorts the folder) and an index would then range from whatever row has
  // since taken that slot.
  const anchorRef = useRef<string | null>(null)

  const ids = useMemo(() => nodes.map((n) => n.id), [nodes])
  const present = useMemo(() => new Set(ids), [ids])

  // Prune during render — React's "adjust state when props change" — rather
  // than in an effect, which would commit one paint with the wrong count and
  // hand a stale id to anything that fires in between.
  let live = selected
  if (selected.size > 0) {
    let dropped = false
    for (const id of selected) {
      if (!present.has(id)) {
        dropped = true
        break
      }
    }
    if (dropped) {
      const kept = new Set<string>()
      for (const id of selected) if (present.has(id)) kept.add(id)
      live = kept
      setSelected(kept)
    }
  }

  // Latest-value refs so the callbacks below can be stable: they read what is
  // current at click time instead of closing over the render that made them.
  const liveRef = useRef(live)
  liveRef.current = live
  const idsRef = useRef(ids)
  idsRef.current = ids

  const isSelected = useCallback((id: string) => liveRef.current.has(id), [])

  const toggle = useCallback((id: string) => {
    setSelected((was) => {
      const next = new Set(was)
      if (!next.delete(id)) next.add(id)
      return next
    })
  }, [])

  const selectAll = useCallback(() => setSelected(new Set(idsRef.current)), [])

  const clear = useCallback(() => {
    anchorRef.current = null
    setSelected(EMPTY)
  }, [])

  const click = useCallback(
    (id: string, mods: ClickModifiers = {}) => {
      const list = idsRef.current
      const anchor = anchorRef.current
      if (mods.shiftKey && anchor !== null) {
        const from = list.indexOf(anchor)
        const to = list.indexOf(id)
        // An anchor that has since left the list has no span to offer; fall
        // through and treat this as the plain click that starts a new one.
        if (from !== -1 && to !== -1) {
          const [lo, hi] = from <= to ? [from, to] : [to, from]
          setSelected(new Set(list.slice(lo, hi + 1)))
          return
        }
      }
      anchorRef.current = id
      if (mods.metaKey || mods.ctrlKey) {
        toggle(id)
        return
      }
      setSelected(new Set([id]))
    },
    [toggle],
  )

  const count = live.size
  const allSelected = ids.length > 0 && count === ids.length
  const someSelected = count > 0 && !allSelected

  return useMemo(
    () => ({ selected: live, count, allSelected, someSelected, isSelected, toggle, selectAll, clear, click }),
    [live, count, allSelected, someSelected, isSelected, toggle, selectAll, clear, click],
  )
}
