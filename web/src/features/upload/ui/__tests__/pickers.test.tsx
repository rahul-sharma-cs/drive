// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { CurrentFolderProvider } from '../../../../app/CurrentFolder'
import { openPicker, UploadPickers } from '../pickers'

// The real actions drive the singleton engine, which would spawn Web Workers
// and open IndexedDB. What this file is about is ingress: which files reach the
// engine, bound to which folder.
const enqueue = vi.fn()
vi.mock('../engineStore', () => ({ uploadActions: { enqueue: (...args: unknown[]) => enqueue(...args) } }))

/**
 * Every case needs a client: a picked tree re-reads the folder when it lands.
 *
 * `moveTo` is the app going somewhere else while the OS chooser is still up —
 * a 401 bouncing through `RequireAuth`, or another tab signing out.
 */
function renderPickers(folderId = 'folder-42') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const tree = (id: string) => (
    <QueryClientProvider client={client}>
      <CurrentFolderProvider folderId={id}>
        <UploadPickers />
      </CurrentFolderProvider>
    </QueryClientProvider>
  )
  const view = render(tree(folderId))
  return { client, ...view, moveTo: (id: string) => view.rerender(tree(id)) }
}

beforeEach(() => {
  enqueue.mockClear()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify({ id: 'folder-1' }), { status: 200 })),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the file pickers', () => {
  it('enqueues picked files against the folder on screen', async () => {
    renderPickers()

    const one = new File(['a'], 'one.txt')
    const two = new File(['b'], 'two.txt')
    await userEvent.upload(screen.getByLabelText('Upload files'), [one, two])

    expect(enqueue.mock.calls).toEqual([
      [one, 'folder-42'],
      [two, 'folder-42'],
    ])
  })

  it('recreates the tree from the folder picker and enqueues under it', async () => {
    renderPickers()

    const nested = new File(['x'], 'report.pdf')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'tree/report.pdf' })
    await userEvent.upload(screen.getByLabelText('Upload folder'), [nested])

    await waitFor(() => expect(enqueue).toHaveBeenCalledWith(nested, 'folder-1'))
    const [url, init] = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0]
    expect(url).toBe('/api/folders')
    // reuse: re-picking a tree must merge into the folders already there.
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      parent_id: 'folder-42',
      name: 'tree',
      conflict_policy: 'reuse',
    })
  })

  it('re-reads the folder on screen once the picked tree has created its folders', async () => {
    const { client } = renderPickers()
    const invalidate = vi.spyOn(client, 'invalidateQueries')

    const nested = new File(['x'], 'report.pdf')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'tree/report.pdf' })
    await userEvent.upload(screen.getByLabelText('Upload folder'), [nested])

    // Without this the new subfolder is invisible until a manual reload: the
    // completion bridge only fires for files, and it invalidates the FILE's
    // parent, which for a nested pick is the subfolder rather than the folder
    // being looked at.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children', 'folder-42'] }))
  })

  it('opens the input the caller asked for, and neither the other one nor both', async () => {
    renderPickers()
    const files = vi.spyOn(screen.getByLabelText('Upload files'), 'click')
    const folder = vi.spyOn(screen.getByLabelText('Upload folder'), 'click')

    openPicker('folder', 'folder-42')

    expect(folder).toHaveBeenCalledTimes(1)
    expect(files).not.toHaveBeenCalled()
  })

  it('enqueues into the folder the picker was opened on, not the one on screen when it closes', async () => {
    const { moveTo } = renderPickers('f9')

    openPicker('files', 'f9')
    moveTo('root-1')

    const one = new File(['a'], 'one.txt')
    await userEvent.upload(screen.getByLabelText('Upload files'), [one])

    // The `change` fires an unbounded time after the click — a person can sit in
    // the OS chooser for a minute — and the destination they chose is the one
    // that was on screen when they opened it.
    expect(enqueue).toHaveBeenCalledWith(one, 'f9')
  })

  it('re-reads the folder the folder picker was opened on, not the one it landed back in', async () => {
    const { client, moveTo } = renderPickers('f9')
    const invalidate = vi.spyOn(client, 'invalidateQueries')

    openPicker('folder', 'f9')
    moveTo('root-1')

    const nested = new File(['x'], 'report.pdf')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'tree/report.pdf' })
    await userEvent.upload(screen.getByLabelText('Upload folder'), [nested])

    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children', 'f9'] }))
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['children', 'root-1'] })
  })
})
