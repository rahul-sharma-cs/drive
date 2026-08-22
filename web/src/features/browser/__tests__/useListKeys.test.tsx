// @vitest-environment jsdom

import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { useListKeys } from '../useListKeys'

interface HarnessProps {
  rows?: string[]
  selected?: string[]
  dialogOpen?: boolean
  onOpen?: (index: number) => void
  onTrash?: (ids: string[]) => void
  onSelectAll?: () => void
  onClear?: () => void
}

/**
 * The smallest list the hook is written for: a `<ul tabIndex={-1}>` carrying
 * `onKeyDown`, focusable rows, and — because Cmd+A is a document listener —
 * a text field and an optional dialog elsewhere on the page to aim it at.
 */
function Harness({
  rows = ['a', 'b', 'c'],
  selected = [],
  dialogOpen = false,
  onOpen = () => {},
  onTrash = () => {},
  onSelectAll = () => {},
  onClear = () => {},
}: HarnessProps) {
  const keys = useListKeys({
    count: rows.length,
    selected: new Set(selected),
    onOpen,
    onTrash,
    onSelectAll,
    onClear,
  })
  return (
    <div>
      <input aria-label="Search" />
      {dialogOpen && <div role="dialog">Rename</div>}
      <span data-testid="focused">{keys.focusedIndex}</span>
      <ul tabIndex={-1} aria-label="Files" onKeyDown={keys.onKeyDown}>
        {rows.map((id, index) => (
          <li key={id} {...keys.rowProps(index)}>
            {id}
          </li>
        ))}
      </ul>
    </div>
  )
}

const list = () => screen.getByRole('list', { name: 'Files' })
const focused = () => Number(screen.getByTestId('focused').textContent)
const press = (key: string, init: Partial<KeyboardEventInit> = {}) => fireEvent.keyDown(list(), { key, ...init })

describe('useListKeys', () => {
  it('moves the roving focus with the arrows and clamps at both ends', () => {
    render(<Harness />)
    expect(focused()).toBe(-1)

    press('ArrowDown')
    expect(focused()).toBe(0)
    // Already at the top: the list does not wrap round to the bottom.
    press('ArrowUp')
    expect(focused()).toBe(0)

    press('ArrowDown')
    press('ArrowDown')
    expect(focused()).toBe(2)
    // Nor past the last row.
    press('ArrowDown')
    expect(focused()).toBe(2)

    press('Home')
    expect(focused()).toBe(0)
    press('End')
    expect(focused()).toBe(2)
  })

  it('marks the focused row and gives it the list’s only tab stop', () => {
    render(<Harness />)
    const rows = screen.getAllByRole('listitem')
    // Nothing focused yet: the first row is the way in.
    expect(rows.map((li) => li.tabIndex)).toEqual([0, -1, -1])
    expect(rows.some((li) => li.hasAttribute('data-focused'))).toBe(false)

    press('ArrowDown')
    press('ArrowDown')
    expect(screen.getAllByRole('listitem').map((li) => li.tabIndex)).toEqual([-1, 0, -1])
    expect(screen.getAllByRole('listitem')[1].hasAttribute('data-focused')).toBe(true)
  })

  it('opens the focused row on Enter and leaves a link inside it alone', () => {
    const onOpen = vi.fn()
    render(
      <div>
        <Harness onOpen={onOpen} />
      </div>,
    )

    // Nothing focused: Enter has no row to open.
    press('Enter')
    expect(onOpen).not.toHaveBeenCalled()

    press('ArrowDown')
    press('ArrowDown')
    press('Enter')
    expect(onOpen.mock.calls).toEqual([[1]])

    // A row's name is a link. Enter on it is the link's own, or the row opens
    // twice.
    const link = document.createElement('a')
    link.href = '/folders/x'
    screen.getAllByRole('listitem')[1].appendChild(link)
    fireEvent.keyDown(link, { key: 'Enter' })
    expect(onOpen.mock.calls).toEqual([[1]])
  })

  it('trashes the whole selection on Delete, and nothing without one', () => {
    const onTrash = vi.fn()
    const { rerender } = render(<Harness selected={['a', 'c']} onTrash={onTrash} />)

    press('Delete')
    expect(onTrash.mock.calls).toEqual([[['a', 'c']]])

    onTrash.mockClear()
    press('Backspace')
    expect(onTrash.mock.calls).toEqual([[['a', 'c']]])

    // With nothing selected there is no target: the focused row is where the
    // arrows happen to be, not a decision to delete it.
    onTrash.mockClear()
    rerender(<Harness selected={[]} onTrash={onTrash} />)
    press('ArrowDown')
    press('Delete')
    press('Backspace')
    expect(onTrash).not.toHaveBeenCalled()
  })

  it('clears the selection on Esc', () => {
    const onClear = vi.fn()
    render(<Harness selected={['a']} onClear={onClear} />)

    press('Escape')
    expect(onClear).toHaveBeenCalledTimes(1)
  })

  it('selects all on Cmd/Ctrl+A from anywhere on the page', () => {
    const onSelectAll = vi.fn()
    render(<Harness onSelectAll={onSelectAll} />)

    // Not on the list, and not even on a row — the point of the document
    // listener is that select-all works with the page focused.
    fireEvent.keyDown(document.body, { key: 'a', metaKey: true })
    expect(onSelectAll).toHaveBeenCalledTimes(1)

    fireEvent.keyDown(document.body, { key: 'a', ctrlKey: true })
    expect(onSelectAll).toHaveBeenCalledTimes(2)

    // Unmodified 'a' is just a letter.
    fireEvent.keyDown(document.body, { key: 'a' })
    expect(onSelectAll).toHaveBeenCalledTimes(2)
  })

  it('leaves Cmd+A alone in a text field', () => {
    const onSelectAll = vi.fn()
    render(<Harness onSelectAll={onSelectAll} />)

    // Cmd+A in the search box means "select this text", and stealing it is how
    // a search query becomes unselectable.
    fireEvent.keyDown(screen.getByLabelText('Search'), { key: 'a', metaKey: true })
    expect(onSelectAll).not.toHaveBeenCalled()
  })

  it('leaves Cmd+A alone while a dialog is open', () => {
    const onSelectAll = vi.fn()
    render(<Harness dialogOpen onSelectAll={onSelectAll} />)

    // Rows behind an open dialog are not what the person is looking at.
    fireEvent.keyDown(document.body, { key: 'a', metaKey: true })
    expect(onSelectAll).not.toHaveBeenCalled()
  })

  it('stops listening on the document once the list unmounts', () => {
    const onSelectAll = vi.fn()
    const { unmount } = render(<Harness onSelectAll={onSelectAll} />)

    unmount()
    fireEvent.keyDown(document.body, { key: 'a', metaKey: true })
    expect(onSelectAll).not.toHaveBeenCalled()
  })
})
