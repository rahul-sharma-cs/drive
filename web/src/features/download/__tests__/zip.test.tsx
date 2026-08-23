// @vitest-environment jsdom

import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useParams } from 'react-router'

import type { DriveNode, Me, Page } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../../browser/FolderPage'
import { collectSubtree, MEMORY_LIMIT, type ZipDeps } from '../zip'
import { resetZipDownload, startZipDownload } from '../useZipDownload'

/**
 * The zip download: the walk, the guard, the cancel, and the click that has to
 * open the save dialog before it does anything else.
 */

/* --------------------------------------------------------------- fixtures */

const { toasts } = vi.hoisted(() => ({ toasts: [] as { message: string; action?: string }[] }))

vi.mock('sonner', () => ({
  toast: {
    error: (message: string, options?: { action?: { label: string } }) =>
      toasts.push({ message, action: options?.action?.label }),
    success: (message: string) => toasts.push({ message }),
  },
}))

const user: Me = {
  id: 'u1',
  email: 'someone@example.test',
  display_name: 'Someone',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
}

const node = (over: Partial<DriveNode>): DriveNode => ({
  id: 'n1',
  parent_id: 'root-1',
  kind: 'folder',
  name: 'Reports',
  size: null,
  mime: null,
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
  ...over,
})

const file = (id: string, name: string, size: number) => node({ id, kind: 'file', name, size })

const page = (items: DriveNode[], next: string | null = null): Page<DriveNode> => ({
  items,
  next_cursor: next,
})

/** Nothing here is real, so the store's host is a documentation one. */
const linkFor = (id: string) => ({ url: `https://store.example/${id}?sig=0123456789abcdef`, expires_at: '2026-01-01T00:00:00Z' })

/** Lets the whole chain of microtasks and timers settle. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0))

function deps(pages: Record<string, Page<DriveNode>>, over: Partial<ZipDeps> = {}): ZipDeps {
  return {
    listChildren: async (id, cursor) => {
      const found = pages[`${id}|${cursor ?? ''}`]
      if (!found) throw new Error(`unstubbed listing: ${id} @ ${cursor ?? 'first page'}`)
      return found
    },
    getDownloadLink: async (id) => linkFor(id),
    fetchBytes: async () => new Response('bytes'),
    ...over,
  }
}

beforeEach(() => {
  toasts.length = 0
  resetZipDownload()
  delete window.showSaveFilePicker
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

/* ------------------------------------------------------------- the walk */

describe('collectSubtree', () => {
  it('follows every page of every folder, keeps the shape, and renames a collision', async () => {
    const roots = [node({ id: 'f1', name: 'Reports' }), file('x1', 'notes.txt', 5)]
    const entries = await collectSubtree(
      roots,
      new AbortController().signal,
      deps({
        // Two pages, and the second one is where the duplicate name lives — a
        // walk that stopped at the cursor would never even see the collision.
        'f1|': page([file('a1', 'a.txt', 10), node({ id: 's1', name: 'Sub' })], 'cursor-2'),
        'f1|cursor-2': page([file('a2', 'a.txt', 20), node({ id: 'e1', name: 'Empty' })]),
        's1|': page([file('d1', 'deep.bin', 30)]),
        'e1|': page([]),
      }),
    )

    expect(entries).toEqual([
      { path: 'Reports/a.txt', id: 'a1', size: 10 },
      // Nesting is the folder's name in the path, not a flattened dump.
      { path: 'Reports/Sub/deep.bin', id: 'd1', size: 30 },
      // Same name, same folder, second page: the suffix goes before the
      // extension so the file still opens as what it is.
      { path: 'Reports/a (1).txt', id: 'a2', size: 20 },
      // No id and a trailing slash: client-zip writes that as a directory, so
      // an empty folder survives the round trip instead of disappearing.
      { path: 'Reports/Empty/', size: 0 },
      { path: 'notes.txt', id: 'x1', size: 5 },
    ])
  })

  it('rejects when a cancel lands between two pages, and asks for nothing more', async () => {
    const ac = new AbortController()
    const pages: Record<string, Page<DriveNode>> = {
      'f1|': page([file('a1', 'a.txt', 10)], 'cursor-2'),
      'f1|cursor-2': page([node({ id: 's1', name: 'Sub' })]),
      's1|': page([file('d1', 'deep.bin', 30)]),
    }

    const listChildren = vi.fn((id: string, cursor?: string) => {
      const answer = Promise.resolve(pages[`${id}|${cursor ?? ''}`])
      if (id === 'f1' && cursor === undefined) {
        // The abort is attached to page 1's promise before the walk awaits it,
        // so it runs on the tick page 1 resolves and before the walk resumes.
        // That is the moment worth testing: a cancel landing *before* page 1
        // came back would be caught by any implementation.
        void answer.then(() => ac.abort())
      }
      return answer
    })

    const walking = collectSubtree([node({ id: 'f1', name: 'Reports' })], ac.signal, deps(pages, { listChildren }))

    // k: page 1 is out, and it is the only thing out.
    const before = listChildren.mock.calls.length
    expect(before).toBe(1)

    // A truncated resolve is the failure this is guarding against — a partial
    // archive that looks whole. It has to reject.
    await expect(walking).rejects.toThrow(/cancel/i)

    await settle()
    expect(listChildren.mock.calls.length).toBe(before)
  })
})

/* ------------------------------------------------------------ the guard */

describe('the size guard, on a browser with no File System Access', () => {
  it('stops above the limit and offers the files one at a time', async () => {
    const getDownloadLink = vi.fn(async (id: string) => linkFor(id))
    startZipDownload([node({ id: 'f1', name: 'Huge' })], {
      deps: deps({ 'f1|': page([file('a1', 'big.bin', MEMORY_LIMIT + 1)]) }, { getDownloadLink }),
    })
    await settle()

    expect(toasts).toHaveLength(1)
    expect(toasts[0].action).toBe('Download files individually')
    // And it really stopped: nothing was asked for, so nothing was held.
    expect(getDownloadLink).not.toHaveBeenCalled()
  })

  it('zips as normal below the limit', async () => {
    const getDownloadLink = vi.fn(async (id: string) => linkFor(id))
    startZipDownload([node({ id: 'f1', name: 'Fine' })], {
      deps: deps({ 'f1|': page([file('a1', 'small.bin', 900_000_000)]) }, { getDownloadLink }),
    })
    await settle()

    expect(toasts).toEqual([])
    expect(getDownloadLink).toHaveBeenCalledWith('a1')
  })
})

/* ------------------------------------------------------- a refused file */

describe('a file the store will not hand over', () => {
  /** A save-file handle whose writer is a spy, so the abort is observable. */
  function stubPicker() {
    const writer = {
      write: vi.fn(async () => {}),
      close: vi.fn(async () => {}),
      abort: vi.fn(async () => {}),
    }
    const picker = vi.fn(async () => ({
      createWritable: async () => ({ getWriter: () => writer }),
    }))
    window.showSaveFilePicker = picker as unknown as typeof window.showSaveFilePicker
    return { writer, picker }
  }

  it('aborts the writer, names the file, and says the archive was not saved', async () => {
    const { writer } = stubPicker()

    startZipDownload([node({ id: 'f1', name: 'Reports' })], {
      deps: deps(
        { 'f1|': page([file('a1', 'fine.txt', 5), file('a2', 'bad.bin', 5)]) },
        { fetchBytes: async (url) => new Response('nope', { status: url.includes('a2') ? 403 : 200 }) },
      ),
    })
    await waitFor(() => expect(toasts).toHaveLength(1))

    expect(toasts[0].message).toContain('bad.bin')
    expect(toasts[0].message).toContain('the archive was not saved')
    // The half-written file is thrown away rather than closed: a named file on
    // disk holding most of an archive is worse than no file at all.
    expect(writer.abort).toHaveBeenCalledTimes(1)
    expect(writer.close).not.toHaveBeenCalled()
  })

  it('closes the writer when every file comes back', async () => {
    const { writer } = stubPicker()

    startZipDownload([node({ id: 'f1', name: 'Reports' })], {
      deps: deps({ 'f1|': page([file('a1', 'fine.txt', 5)]) }),
    })
    await waitFor(() => expect(writer.close).toHaveBeenCalledTimes(1))

    expect(toasts).toEqual([])
    expect(writer.abort).not.toHaveBeenCalled()
  })
})

/* ------------------------------------------------ one archive at a time */

describe('a second request while one is running', () => {
  it('is refused rather than queued', async () => {
    const stub = deps({ 'f1|': page([file('a1', 'a.txt', 5)]) })
    const roots = [node({ id: 'f1', name: 'Reports' })]

    startZipDownload(roots, { deps: stub })
    startZipDownload(roots, { deps: stub })

    expect(toasts).toEqual([{ message: 'One archive at a time', action: undefined }])
    await settle()
  })
})

/* ------------------------------------------------------- the click path */

const twoFilesAndAFolder: StubRoute[] = [
  { path: '/api/nodes/root-1', body: node({ id: 'root-1', parent_id: null, name: 'root' }) },
  {
    path: '/api/nodes/root-1/children',
    body: page([
      node({ id: 'f1', name: 'Reports' }),
      file('x1', 'notes.txt', 2048),
      file('x2', 'memo.txt', 1024),
    ]),
  },
]

function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
    </CurrentFolderProvider>
  )
}

function renderFolder(routes: StubRoute[]) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
      </Route>
    </Routes>,
    { seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const bar = () => screen.getByRole('toolbar', { name: 'Selection actions' })
const select = (name: string) => userEvent.click(screen.getByRole('checkbox', { name: `Select ${name}` }))

describe('what the band offers', () => {
  it('downloads one file as itself and anything else as an archive', async () => {
    renderFolder(twoFilesAndAFolder)
    await screen.findByText('notes.txt')

    // One file: still a plain navigation to the 302, so the browser's own
    // download handles it. Wrapping it in a zip would be a loss.
    await select('notes.txt')
    expect(within(bar()).getByRole('link', { name: 'Download' }).getAttribute('href')).toBe(
      '/api/files/x1/download',
    )

    // Two files: no single URL to point at, so it becomes an archive — a
    // command, not a link.
    await select('memo.txt')
    expect(within(bar()).queryByRole('link', { name: 'Download' })).toBeNull()
    expect(within(bar()).getByRole('button', { name: 'Download' })).toBeTruthy()

    // A folder on its own: same answer.
    await userEvent.click(screen.getByRole('button', { name: 'Clear the selection' }))
    await select('Reports')
    expect(within(bar()).queryByRole('link', { name: 'Download' })).toBeNull()
    expect(within(bar()).getByRole('button', { name: 'Download' })).toBeTruthy()
  })

  it('puts Download on a folder’s kebab too', async () => {
    renderFolder(twoFilesAndAFolder)
    await screen.findByText('Reports')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Reports' }))
    const download = await screen.findByRole('menuitem', { name: 'Download' })
    // A command rather than an `href`: a folder has no URL of its own to fetch.
    expect(download.getAttribute('href')).toBeNull()
  })

  it('opens the save dialog inside the click, before it asks the API anything', async () => {
    const picker = vi.fn(() => new Promise<FileSystemFileHandle>(() => {}))
    window.showSaveFilePicker = picker as unknown as typeof window.showSaveFilePicker

    const { calls } = renderFolder(twoFilesAndAFolder)
    await screen.findByText('Reports')
    await select('Reports')

    const before = calls.length
    // `fireEvent`, not `userEvent`: this assertion is about what has happened by
    // the time the click handler returns, and `userEvent` awaits past it.
    fireEvent.click(within(bar()).getByRole('button', { name: 'Download' }))

    // Transient user activation is gone by the first await, and the paginated
    // walk is all awaits — so the picker has to be spent here or not at all.
    expect(picker).toHaveBeenCalledTimes(1)
    expect(calls.length).toBe(before)
  })
})
