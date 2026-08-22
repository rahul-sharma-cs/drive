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

/** Every case needs a client: a picked tree re-reads the folder when it lands. */
function renderPickers(folderId = 'folder-42') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const view = render(
    <QueryClientProvider client={client}>
      <CurrentFolderProvider folderId={folderId}>
        <UploadPickers />
      </CurrentFolderProvider>
    </QueryClientProvider>,
  )
  return { client, ...view }
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

    openPicker('folder')

    expect(folder).toHaveBeenCalledTimes(1)
    expect(files).not.toHaveBeenCalled()
  })
})
