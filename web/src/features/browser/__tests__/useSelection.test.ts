// @vitest-environment jsdom

import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { DriveNode } from '../../../lib/api'
import { useSelection } from '../useSelection'

const node = (id: string): DriveNode => ({
  id,
  parent_id: 'root-1',
  kind: 'file',
  name: `${id}.txt`,
  size: 1024,
  mime: 'text/plain',
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
})

const five = ['a', 'b', 'c', 'd', 'e'].map(node)

function mount(nodes: DriveNode[] = five) {
  return renderHook(({ rows }: { rows: DriveNode[] }) => useSelection(rows), { initialProps: { rows: nodes } })
}

const ids = (set: ReadonlySet<string>) => [...set].sort()

describe('useSelection', () => {
  it('selects only the row a plain click landed on', () => {
    const { result } = mount()

    act(() => result.current.click('b'))
    expect(ids(result.current.selected)).toEqual(['b'])
    expect(result.current.count).toBe(1)
    expect(result.current.isSelected('b')).toBe(true)

    // The second click replaces rather than accumulating — an unmodified click
    // is "I mean this one", not "and also this one".
    act(() => result.current.click('d'))
    expect(ids(result.current.selected)).toEqual(['d'])
  })

  it('toggles under cmd/ctrl without clearing what is already selected', () => {
    const { result } = mount()

    act(() => result.current.click('b'))
    act(() => result.current.click('d', { metaKey: true }))
    expect(ids(result.current.selected)).toEqual(['b', 'd'])

    act(() => result.current.click('a', { ctrlKey: true }))
    expect(ids(result.current.selected)).toEqual(['a', 'b', 'd'])

    // The same modified click on a selected row takes it back out, and leaves
    // the others alone.
    act(() => result.current.click('b', { metaKey: true }))
    expect(ids(result.current.selected)).toEqual(['a', 'd'])
  })

  it('shift-clicks the inclusive span and re-ranges from the same anchor', () => {
    const { result } = mount()

    act(() => result.current.click('b'))
    act(() => result.current.click('d', { shiftKey: true }))
    expect(ids(result.current.selected)).toEqual(['b', 'c', 'd'])

    // A second shift-click ranges from the original anchor again — it does not
    // start from the row the last shift-click ended on, which is what turns a
    // correction into an ever-growing selection.
    act(() => result.current.click('e', { shiftKey: true }))
    expect(ids(result.current.selected)).toEqual(['b', 'c', 'd', 'e'])

    // Backwards over the anchor, and shrinking, both work for the same reason.
    act(() => result.current.click('a', { shiftKey: true }))
    expect(ids(result.current.selected)).toEqual(['a', 'b'])
  })

  it('leaves the anchor where it was when a checkbox toggles a row', () => {
    const { result } = mount()

    act(() => result.current.click('b'))
    act(() => result.current.toggle('a'))
    expect(ids(result.current.selected)).toEqual(['a', 'b'])

    // The anchor is still 'b', so the span is b..d and not a..d.
    act(() => result.current.click('d', { shiftKey: true }))
    expect(ids(result.current.selected)).toEqual(['b', 'c', 'd'])
  })

  it('drops ids that leave the list and keeps the rest', () => {
    const { result, rerender } = mount()

    act(() => result.current.click('b'))
    act(() => result.current.click('d', { metaKey: true }))
    expect(ids(result.current.selected)).toEqual(['b', 'd'])

    // 'b' is trashed: the next page of children arrives without it.
    rerender({ rows: five.filter((n) => n.id !== 'b') })
    expect(ids(result.current.selected)).toEqual(['d'])
    expect(result.current.count).toBe(1)
    expect(result.current.isSelected('b')).toBe(false)
  })

  it('reports allSelected and someSelected for the header checkbox', () => {
    const empty = mount([])
    expect(empty.result.current.allSelected).toBe(false)
    expect(empty.result.current.someSelected).toBe(false)
    expect(empty.result.current.count).toBe(0)

    const { result } = mount()
    expect(result.current.allSelected).toBe(false)
    expect(result.current.someSelected).toBe(false)

    act(() => result.current.click('a'))
    expect(result.current.allSelected).toBe(false)
    expect(result.current.someSelected).toBe(true)

    act(() => result.current.selectAll())
    expect(result.current.allSelected).toBe(true)
    expect(result.current.someSelected).toBe(false)
  })

  it('selects every row and clears back to nothing', () => {
    const { result } = mount()

    act(() => result.current.selectAll())
    expect(ids(result.current.selected)).toEqual(['a', 'b', 'c', 'd', 'e'])
    expect(result.current.count).toBe(5)

    act(() => result.current.clear())
    expect(result.current.count).toBe(0)
    expect(result.current.isSelected('a')).toBe(false)
  })

  it('keeps its callbacks stable so rows can be memoised', () => {
    const { result } = mount()
    const before = result.current

    act(() => result.current.click('a'))

    expect(result.current.click).toBe(before.click)
    expect(result.current.toggle).toBe(before.toggle)
    expect(result.current.selectAll).toBe(before.selectAll)
    expect(result.current.clear).toBe(before.clear)
    expect(result.current.isSelected).toBe(before.isSelected)
  })
})
