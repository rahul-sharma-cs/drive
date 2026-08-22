/**
 * Keyboard handling for a file list.
 *
 * The list is a `<ul tabIndex={-1}>` whose rows are focusable; `onKeyDown` goes
 * on the `<ul>` and catches what bubbles up from the rows. Focus is roving —
 * one row carries `tabIndex={0}` and the rest `-1` — so the list is a single
 * tab stop and the arrows move inside it.
 *
 * Cmd/Ctrl+A is the exception: select-all has to work while the focus is on the
 * page rather than inside the list, so it is a `document` listener. That makes
 * it everyone's keystroke, hence the two guards — never steal it from a text
 * field, and never fire it under an open menu or dialog, where Cmd+A means
 * "select this text" and Delete would act on rows the person cannot see.
 *
 * A menu opens in a portal on `document.body`, but React sends SYNTHETIC
 * events along the component tree, so a keydown inside a row's menu still
 * arrives here. Every key below is therefore gated on the event having come
 * from inside the list itself.
 *
 * The hook owns no DOM. It reports which row index is focused; moving the
 * browser's own focus there (and any `aria-activedescendant`) is the list's
 * job, since only the list holds the row elements.
 */

import { useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'

export interface ListKeysOptions {
  /** How many rows the list is showing. The focused index is clamped to it. */
  count: number
  /** The ids the command keys act on — `useSelection`'s `selected`, unchanged. */
  selected: ReadonlySet<string>
  /** Enter on the focused row. */
  onOpen: (index: number) => void
  /** Delete/Backspace. Never called with an empty list of ids. */
  onTrash: (ids: string[]) => void
  /**
   * Cmd/Ctrl+A. Answers whether the list actually took the key: a list that
   * cannot select has not handled it, and the browser's own Select All is left
   * alone rather than swallowed.
   */
  onSelectAll: () => boolean
  /** Esc. */
  onClear: () => void
}

export interface RowKeyProps {
  tabIndex: number
  'data-focused': '' | undefined
}

export interface ListKeys {
  /** The row the arrows are on, or -1 before the list has been entered. */
  focusedIndex: number
  /** Moves the roving focus; the value is clamped into the list. */
  setFocusedIndex: (index: number) => void
  /** Goes on the `<ul>`. */
  onKeyDown: (event: ReactKeyboardEvent<HTMLElement>) => void
  /** Roving `tabIndex` plus a `data-focused` hook for styling row `index`. */
  rowProps: (index: number) => RowKeyProps
}

/** True when the event landed in something the person is typing into. */
function isEditable(target: EventTarget | null): boolean {
  const el = target as Element | null
  if (!el || typeof el.closest !== 'function') return false
  return el.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]') !== null
}

/** True when the browser will act on Enter itself, so we must not act too. */
function isActivatable(target: EventTarget | null): boolean {
  const el = target as Element | null
  if (!el || typeof el.closest !== 'function') return false
  return el.closest('a[href], button') !== null
}

/**
 * True when the key was pressed inside the list rather than in something the
 * list merely contains in the React tree.
 *
 * Radix portals a row's kebab and right-click menus to `document.body`, and
 * React's synthetic events follow the component tree rather than the DOM, so
 * without this Delete inside an open menu trashed the selection behind it,
 * Enter on a menu item opened the focused row as well as the item, and Esc
 * dismissed the menu and cleared the selection in one press.
 */
function insideList(event: ReactKeyboardEvent<HTMLElement>): boolean {
  const target = event.target as Node | null
  return target !== null && event.currentTarget.contains(target)
}

/**
 * True while a Radix portal is open. Menus and dialogs carry the roles; the
 * `data-slot` clause is the backstop for the shadcn primitives, whose content
 * parts are all named `*-content` and carry Radix's `data-state="open"` (a
 * tooltip reads `delayed-open`/`instant-open`, so hovering something does not
 * disable select-all).
 */
function portalOpen(): boolean {
  return (
    document.querySelector('[role="dialog"], [role="menu"], [data-state="open"][data-slot$="-content"]') !==
    null
  )
}

export function useListKeys({ count, selected, onOpen, onTrash, onSelectAll, onClear }: ListKeysOptions): ListKeys {
  const [focusedIndex, setFocused] = useState(-1)

  // Clamp during render, so a list that shrank under the cursor (rows trashed,
  // a search narrowed) never reports a row index that no longer exists.
  let focused = focusedIndex
  if (focused > count - 1) {
    focused = count - 1
    setFocused(focused)
  }

  // Latest-value refs: the `<ul>` handler and the document listener are both
  // registered once and read what is current when a key is actually pressed.
  const countRef = useRef(count)
  countRef.current = count
  const focusedRef = useRef(focused)
  focusedRef.current = focused
  const handlers = useRef({ selected, onOpen, onTrash, onSelectAll, onClear })
  handlers.current = { selected, onOpen, onTrash, onSelectAll, onClear }

  const clamp = useCallback((index: number) => Math.max(-1, Math.min(countRef.current - 1, index)), [])

  const setFocusedIndex = useCallback((index: number) => setFocused(clamp(index)), [clamp])

  const move = useCallback((delta: number) => {
    setFocused((was) => {
      const last = countRef.current - 1
      if (last < 0) return -1
      // Entering the list from nowhere lands on the near end, so ArrowUp with
      // nothing focused reaches the bottom row instead of doing nothing.
      if (was < 0) return delta > 0 ? 0 : last
      return Math.max(0, Math.min(last, was + delta))
    })
  }, [])

  const onKeyDown = useCallback(
    (event: ReactKeyboardEvent<HTMLElement>) => {
      if (isEditable(event.target)) return
      if (!insideList(event)) return
      // Something inside the list has already acted on this key and said so.
      if (event.defaultPrevented) return
      const { selected: ids, onOpen: open, onTrash: trash, onClear: clear } = handlers.current
      switch (event.key) {
        case 'ArrowDown':
          event.preventDefault()
          move(1)
          break
        case 'ArrowUp':
          event.preventDefault()
          move(-1)
          break
        case 'Home':
          event.preventDefault()
          setFocused(countRef.current > 0 ? 0 : -1)
          break
        case 'End':
          event.preventDefault()
          setFocused(countRef.current - 1)
          break
        case 'Enter': {
          // A row's name is a link; Enter on it is the link's, and opening
          // twice would both navigate and open the preview.
          if (isActivatable(event.target)) return
          const index = focusedRef.current
          if (index < 0 || index >= countRef.current) return
          event.preventDefault()
          open(index)
          break
        }
        case 'Delete':
        case 'Backspace': {
          // No selection means no target: the focused row is where the arrows
          // happen to be, which is not a decision to delete anything.
          if (ids.size === 0) return
          event.preventDefault()
          trash([...ids])
          break
        }
        case 'Escape':
          event.preventDefault()
          clear()
          break
      }
    },
    [move],
  )

  useEffect(() => {
    const onDocumentKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key !== 'a' && event.key !== 'A') return
      if (!(event.metaKey || event.ctrlKey) || event.altKey) return
      if (isEditable(event.target)) return
      if (portalOpen()) return
      // Ask first, swallow second: on a list with no selection this key is
      // still the browser's, and taking it there buys nothing.
      if (!handlers.current.onSelectAll()) return
      event.preventDefault()
    }
    document.addEventListener('keydown', onDocumentKeyDown)
    return () => document.removeEventListener('keydown', onDocumentKeyDown)
  }, [])

  const rowProps = useCallback(
    (index: number): RowKeyProps => ({
      // With nothing focused the first row is the tab stop, so Tab can still
      // reach the list.
      tabIndex: index === (focused < 0 ? 0 : focused) ? 0 : -1,
      'data-focused': index === focused ? '' : undefined,
    }),
    [focused],
  )

  return { focusedIndex: focused, setFocusedIndex, onKeyDown, rowProps }
}
