// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { UploadSnapshot } from '../../engine/types'
import { useCompletionBridge } from '../UploadDock'

const success = vi.fn()
vi.mock('sonner', () => ({ toast: { success: (...args: unknown[]) => success(...args), error: vi.fn() } }))

function item(over: Partial<UploadSnapshot> = {}): UploadSnapshot {
  return {
    id: 'u1',
    upload_id: 'srv-1',
    name: 'notes.txt',
    original_name: 'notes.txt',
    renamed: false,
    size: 10,
    parent_id: 'folder-7',
    state: 'uploading',
    progress: 0.5,
    bytes_confirmed: 5,
    parts_total: 2,
    parts_confirmed: 1,
    speed_bps: null,
    eta_seconds: null,
    error_code: null,
    error: null,
    session_expires_at: null,
    node_id: null,
    verify_parts: null,
    ...over,
  }
}

function Harness({ items }: { items: UploadSnapshot[] }) {
  useCompletionBridge(items)
  return null
}

function mount(items: UploadSnapshot[]) {
  const client = new QueryClient()
  const invalidate = vi.spyOn(client, 'invalidateQueries')
  const view = render(
    <QueryClientProvider client={client}>
      <Harness items={items} />
    </QueryClientProvider>,
  )
  const rerender = (next: UploadSnapshot[]) =>
    view.rerender(
      <QueryClientProvider client={client}>
        <Harness items={next} />
      </QueryClientProvider>,
    )
  return { invalidate, rerender }
}

describe('completion bridge', () => {
  it('re-reads the destination folder when an upload publishes', () => {
    const { invalidate, rerender } = mount([item()])
    // Nothing has finished yet, so nothing is re-read.
    expect(invalidate).not.toHaveBeenCalled()

    rerender([item({ state: 'done', progress: 1 })])

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children', 'folder-7'] })
    expect(success).toHaveBeenCalledWith('Uploaded notes.txt')
  })

  it('announces the server-chosen name when a collision was auto-renamed', () => {
    const { rerender } = mount([item()])
    success.mockClear()

    rerender([item({ state: 'done', progress: 1, name: 'notes (1).txt', renamed: true })])

    expect(success).toHaveBeenCalledWith(expect.stringContaining('notes (1).txt'))
    expect(success).toHaveBeenCalledWith(expect.stringContaining('notes.txt'))
  })

  it('fires once per upload, not on every later snapshot', () => {
    const { invalidate, rerender } = mount([item()])
    const done = item({ state: 'done', progress: 1 })

    rerender([done])
    rerender([done])
    rerender([{ ...done }])

    // Two invalidations per completion — the folder and the session list — and
    // the row staying `done` in later snapshots must not repeat them.
    expect(invalidate).toHaveBeenCalledTimes(2)
  })
})
