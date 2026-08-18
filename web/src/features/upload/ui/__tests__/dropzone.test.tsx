// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { DropZone } from '../DropZone'

// The real actions drive the singleton engine, which would spawn Web Workers
// and open IndexedDB. What this file is about is ingress: which files reach the
// engine, bound to which folder.
const enqueue = vi.fn()
vi.mock('../engineStore', () => ({ uploadActions: { enqueue: (...args: unknown[]) => enqueue(...args) } }))

/** Every case needs a client: the drop re-reads the folder when it finishes. */
function renderZone(folderId = 'folder-42') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const view = render(
    <QueryClientProvider client={client}>
      <DropZone folderId={folderId}>
        <p>rows</p>
      </DropZone>
    </QueryClientProvider>,
  )
  return { client, ...view }
}

function fileEntry(file: File): FileSystemEntry {
  return {
    isFile: true,
    isDirectory: false,
    name: file.name,
    file: (cb: (f: File) => void) => cb(file),
  } as unknown as FileSystemEntry
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

describe('drop zone', () => {
  it('enqueues picked files against the folder on screen', async () => {
    renderZone()

    const one = new File(['a'], 'one.txt')
    const two = new File(['b'], 'two.txt')
    await userEvent.upload(screen.getByLabelText('Upload files'), [one, two])

    expect(enqueue.mock.calls).toEqual([
      [one, 'folder-42'],
      [two, 'folder-42'],
    ])
  })

  it('collects drop entries synchronously, before the item list dies', async () => {
    renderZone()

    const dropped = new File(['x'], 'photo.jpg')
    const items = [{ kind: 'file', webkitGetAsEntry: () => fileEntry(dropped) }]
    fireEvent.drop(screen.getByTestId('drop-zone'), { dataTransfer: { types: ['Files'], items } })
    // A DataTransfer's items are invalidated the moment the handler yields.
    // Emptying them here — synchronously after dispatch — is what a real
    // browser does, so a handler that awaited before collecting sees nothing.
    items.length = 0

    await waitFor(() => expect(enqueue).toHaveBeenCalledWith(dropped, 'folder-42'))
  })

  it('re-reads the folder on screen once the drop has created its folders', async () => {
    const { client } = renderZone()
    const invalidate = vi.spyOn(client, 'invalidateQueries')

    // An EMPTY dropped folder is the case that makes this load-bearing: it
    // enqueues no upload, so the completion bridge never fires, and without
    // this the new folder is invisible until a manual reload. A nested drop is
    // no better — the bridge invalidates the FILE's parent, which is the new
    // subfolder, not the folder being looked at.
    const dir = {
      isFile: false,
      isDirectory: true,
      name: 'empty',
      createReader: () => ({ readEntries: (cb: (e: FileSystemEntry[]) => void) => cb([]) }),
    } as unknown as FileSystemEntry
    fireEvent.drop(screen.getByTestId('drop-zone'), {
      dataTransfer: { types: ['Files'], items: [{ kind: 'file', webkitGetAsEntry: () => dir }] },
    })

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children', 'folder-42'] }))
    expect(enqueue).not.toHaveBeenCalled()
  })

  it('ignores a drag that is a row moving inside the app, not files arriving', async () => {
    // A row dragged towards another folder used to light this panel up with
    // "Drop to upload here" and hand its own hyperlink to the upload engine.
    // Only a drag whose types include `Files` is an upload.
    renderZone()
    const zone = screen.getByTestId('drop-zone')
    const dragged = new File(['x'], 'never-read.jpg')

    fireEvent.dragOver(zone, { dataTransfer: { types: ['application/x-drive-node'] } })
    expect(screen.queryByText('Drop to upload here')).toBeNull()

    fireEvent.drop(zone, {
      dataTransfer: {
        types: ['application/x-drive-node'],
        items: [{ kind: 'file', webkitGetAsEntry: () => fileEntry(dragged) }],
      },
    })
    await waitFor(() => expect(enqueue).not.toHaveBeenCalled())
  })

  it('shows the drop target only while files are actually over it', async () => {
    renderZone()
    const zone = screen.getByTestId('drop-zone')

    fireEvent.dragOver(zone, { dataTransfer: { types: ['Files'] } })
    expect(screen.getByText('Drop to upload here')).toBeTruthy()
  })

  it('recreates the tree from the folder picker and enqueues under it', async () => {
    renderZone()

    const nested = new File(['x'], 'report.pdf')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'tree/report.pdf' })
    await userEvent.upload(screen.getByLabelText('Upload folder'), [nested])

    await waitFor(() => expect(enqueue).toHaveBeenCalledWith(nested, 'folder-1'))
    const [url, init] = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0]
    expect(url).toBe('/api/folders')
    // reuse: re-dropping a tree must merge into the folders already there.
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      parent_id: 'folder-42',
      name: 'tree',
      conflict_policy: 'reuse',
    })
  })
})
