// @vitest-environment jsdom

import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useParams } from 'react-router'

import type { DriveNode, Me, Page } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../../browser/FolderPage'
import { archiveBytes, collectSubtree, liveDeps, MEMORY_LIMIT, writeArchive, type ZipDeps, type ZipSink } from '../zip'
import { resetZipDownload, startZipDownload } from '../useZipDownload'
import { ZipDock } from '../ZipDock'

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

/**
 * A save-file handle whose writer and `remove` are spies, so both halves of a
 * failed archive — the bytes thrown away, the named file deleted — are
 * observable.
 */
function stubPicker() {
  const writer = {
    write: vi.fn(async () => {}),
    close: vi.fn(async () => {}),
    abort: vi.fn(async () => {}),
  }
  const handle = {
    createWritable: async () => ({ getWriter: () => writer }),
    remove: vi.fn(async () => {}),
  }
  const picker = vi.fn(async () => handle)
  window.showSaveFilePicker = picker as unknown as typeof window.showSaveFilePicker
  return { writer, handle, picker }
}

/** Starts an archive and lets everything it kicked off settle, inside one `act`. */
async function start(roots: readonly DriveNode[], stub: ZipDeps): Promise<void> {
  await act(async () => {
    startZipDownload(roots, { deps: stub })
    await settle()
  })
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

  it('renames a collision that is only a difference in case', async () => {
    // Two roots, not two siblings: the server's sibling index is on lower(name)
    // so these two cannot share a folder, but a selection made in search puts
    // them in one archive folder anyway. A zip may hold both — macOS and
    // Windows extract them onto one case-insensitive filesystem, where the
    // second overwrites the first and one of the two files is simply gone.
    const entries = await collectSubtree(
      [file('r1', 'Report.txt', 10), file('r2', 'report.txt', 20)],
      new AbortController().signal,
      deps({}),
    )

    // The rename keeps the casing it was given; only the collision test folds.
    expect(entries).toEqual([
      { path: 'Report.txt', id: 'r1', size: 10 },
      { path: 'report (1).txt', id: 'r2', size: 20 },
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

describe('the size guard', () => {
  /** The walk that trips it: one huge file, a small one, and one a folder down. */
  const tooBig = {
    'f1|': page([
      file('a1', 'big.bin', MEMORY_LIMIT + 1),
      file('a2', 'notes.txt', 12),
      node({ id: 's1', name: 'Sub' }),
    ]),
    's1|': page([file('a3', 'deep.bin', 7)]),
  }

  it('offers every walked file as its own link when there is nowhere to stream to', async () => {
    const getDownloadLink = vi.fn(async (id: string) => linkFor(id))
    renderApp(<ZipDock />)

    await start([node({ id: 'f1', name: 'Huge' })], deps(tooBig, { getDownloadLink }))

    const dialog = screen.getByRole('dialog', { name: 'Too large to zip in this browser' })
    // Every file the walk found, in walk order, each pointing at the app's own
    // 302 — one link, one click, one download. Not a timed loop of clicks: the
    // browsers this path exists for spend the click's activation on the first
    // one and block the rest as a popup storm, which delivered one file out of
    // however many and said nothing about the others.
    expect(within(dialog).getAllByRole('link').map((link) => link.getAttribute('href'))).toEqual([
      '/api/files/a1/download',
      '/api/files/a2/download',
      '/api/files/a3/download',
    ])
    // The folder the file sat in is the only thing telling two same-named files
    // apart once the archive is gone, so the row shows the whole path.
    expect(within(dialog).getByRole('link', { name: /Sub\/deep\.bin/ })).toBeTruthy()

    // Nothing was fetched and nothing was said: the offer *is* the message.
    expect(getDownloadLink).not.toHaveBeenCalled()
    expect(toasts).toEqual([])

    // A link is a link. Clicking one downloads that file and leaves the rest of
    // the list standing — it does not start the archive that was just refused.
    fireEvent.click(within(dialog).getByRole('link', { name: /notes\.txt/ }))
    await settle()
    expect(getDownloadLink).not.toHaveBeenCalled()
    expect(screen.queryByRole('progressbar')).toBeNull()
    expect(screen.getByRole('dialog')).toBeTruthy()

    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('zips above the limit when there is somewhere to stream it', async () => {
    // The limit is about holding an archive in memory, not about size: with the
    // picker there, the bytes go to disk as they arrive and nothing is held.
    stubPicker()
    const getDownloadLink = vi.fn(async (id: string) => linkFor(id))
    renderApp(<ZipDock />)

    await start([node({ id: 'f1', name: 'Huge' })], deps(tooBig, { getDownloadLink }))

    expect(getDownloadLink).toHaveBeenCalledWith('a1')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(toasts).toEqual([])
  })

  it('zips as normal below the limit', async () => {
    const getDownloadLink = vi.fn(async (id: string) => linkFor(id))
    renderApp(<ZipDock />)

    await start([node({ id: 'f1', name: 'Fine' })], deps({ 'f1|': page([file('a1', 'small.bin', 900_000_000)]) }, { getDownloadLink }))

    expect(toasts).toEqual([])
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(getDownloadLink).toHaveBeenCalledWith('a1')
  })
})

/* ------------------------------------------------------- a refused file */

describe('a file the store will not hand over', () => {
  it('aborts the writer, deletes the file, and names both files in the toast', async () => {
    const { writer, handle } = stubPicker()

    startZipDownload([node({ id: 'f1', name: 'Reports' })], {
      deps: deps(
        { 'f1|': page([file('a1', 'fine.txt', 5), file('a2', 'bad.bin', 5)]) },
        { fetchBytes: async (url) => new Response('nope', { status: url.includes('a2') ? 403 : 200 }) },
      ),
    })
    await waitFor(() => expect(toasts).toHaveLength(1))

    expect(toasts[0].message).toContain('bad.bin')
    // The archive is named too: the browser created that file the moment the
    // dialog was answered, and on a browser without `remove()` an empty one
    // survives the abort. Whoever reads this has to know which file is the dud.
    expect(toasts[0].message).toContain('“Reports.zip” was not saved')
    // The half-written file is thrown away rather than closed: a named file on
    // disk holding most of an archive is worse than no file at all.
    expect(writer.abort).toHaveBeenCalledTimes(1)
    expect(writer.close).not.toHaveBeenCalled()
    // Aborting the writer discards the bytes, not the file — without this the
    // failure leaves a 0-byte Reports.zip sitting where the real one would be.
    expect(handle.remove).toHaveBeenCalledTimes(1)
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

/* --------------------------------------------------------- the byte fetch */

describe('the fetch that reads the store', () => {
  it('carries the abort signal and nothing else', async () => {
    const fetchMock = vi.fn((_url: string, _init?: RequestInit) => Promise.resolve(new Response('bytes')))
    vi.stubGlobal('fetch', fetchMock)
    const { signal } = new AbortController()
    const url = `${linkFor('a1').url}`

    await liveDeps.fetchBytes(url, signal)

    // The signal is what a cancelled archive pulls to stop bytes mid-flight.
    // Everything else is what must *not* be there: a header of any kind turns
    // this cross-origin GET into a preflight, and the store's CORS rule answers
    // a preflight it did not enumerate with a 403 — the download fails, having
    // never reached the object.
    expect(fetchMock).toHaveBeenCalledWith(url, { signal })
    // Spelled out separately because `toHaveBeenCalledWith` ignores keys whose
    // value is `undefined`, and `{ signal, credentials: undefined }` is still a
    // second init key someone added.
    expect(Object.keys(fetchMock.mock.calls[0][1] ?? {})).toEqual(['signal'])
  })
})

/* ----------------------------------------------------------- the progress */

describe('progress reporting', () => {
  /** A stream that arrives in `count` chunks, the way a real body does. */
  const chunked = (count: number, size: number) =>
    new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          for (let i = 0; i < count; i++) controller.enqueue(new Uint8Array(size))
          controller.close()
        },
      }),
    )

  it('coalesces a stream of chunks into a few reports, and ends on the exact total', async () => {
    // Date is frozen rather than mocked away: it isolates the byte rule from
    // the time rule, so the count below is the coalescing and not the speed of
    // the machine running the test.
    vi.useFakeTimers({ toFake: ['Date'] })
    try {
      const entries = [{ path: 'big.bin', id: 'a1', size: 100 * 1024 }]
      let bytes = 0
      const sink: ZipSink = {
        write: async (chunk) => {
          bytes += chunk.byteLength
        },
        close: async () => {},
        abort: async () => {},
      }
      const reports: number[] = []

      await writeArchive(
        entries,
        sink,
        new AbortController().signal,
        deps({}, { fetchBytes: async () => chunked(100, 1024) }),
        ({ written }) => reports.push(written),
      )

      // 100 chunks in, a handful of reports out — every one of them is a React
      // render of the dock for a number that has not visibly moved.
      expect(reports.length).toBeLessThan(10)
      // And the last one is the truth, not a coalesced number a chunk behind:
      // the dock's final frame has to read as finished.
      expect(reports.at(-1)).toBe(bytes)
      expect(reports.at(-1)).toBe(archiveBytes(entries))
    } finally {
      vi.useRealTimers()
    }
  })
})

/* -------------------------------------------------------- the save dialog */

describe('the save dialog', () => {
  it('does not hold the walk up while it is open', async () => {
    // A person choosing a folder takes seconds; the listing needs nothing from
    // them. Waiting for the handle first spends that time twice.
    window.showSaveFilePicker = vi.fn(
      () => new Promise<FileSystemFileHandle>(() => {}),
    ) as unknown as typeof window.showSaveFilePicker
    const listChildren = vi.fn(async () => page([file('a1', 'a.txt', 5)]))

    startZipDownload([node({ id: 'f1', name: 'Reports' })], { deps: deps({}, { listChildren }) })
    await settle()

    expect(listChildren).toHaveBeenCalledWith('f1', undefined)
  })

  it('says nothing when it is dismissed, and stops the walk it started', async () => {
    window.showSaveFilePicker = vi.fn(async () => {
      throw new DOMException('The user aborted a request.', 'AbortError')
    }) as unknown as typeof window.showSaveFilePicker
    // A chain of folders, deep enough that a walk nobody stopped would still be
    // asking for pages long after the dialog was dismissed.
    const listChildren = vi.fn(async (id: string) => {
      const depth = Number(id.slice(1))
      return depth === 8 ? page([]) : page([node({ id: `f${depth + 1}`, name: 'Sub' })])
    })
    renderApp(<ZipDock />)

    await start([node({ id: 'f1', name: 'Reports' })], deps({}, { listChildren }))

    // Dismissing a dialog is an answer, not an error to report back.
    expect(toasts).toEqual([])
    expect(screen.queryByRole('progressbar')).toBeNull()
    // The walk was already running when the dialog came back empty-handed, and
    // the dismissal stopped it: it got a folder or two in, not all eight.
    expect(listChildren).toHaveBeenCalledWith('f1', undefined)
    expect(listChildren.mock.calls.length).toBeLessThan(4)
  })

  it('says one plain sentence when the browser refuses to open it', async () => {
    // What the browser says here is "Failed to execute 'showSaveFilePicker' on
    // 'Window': …" — a console string, and thrown synchronously at that.
    window.showSaveFilePicker = vi.fn(() => {
      throw new TypeError("Failed to execute 'showSaveFilePicker' on 'Window'")
    }) as unknown as typeof window.showSaveFilePicker

    startZipDownload([node({ id: 'f1', name: 'Reports' })], {
      deps: deps({ 'f1|': page([file('a1', 'a.txt', 5)]) }),
    })
    await waitFor(() => expect(toasts).toHaveLength(1))

    expect(toasts[0].message).toBe('Couldn’t open the save dialog — try again from a click')
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
    let callsWhenOpened = -1
    const picker = vi.fn(() => {
      callsWhenOpened = calls.length
      return new Promise<FileSystemFileHandle>(() => {})
    })
    window.showSaveFilePicker = picker as unknown as typeof window.showSaveFilePicker

    const { calls } = renderFolder([
      ...twoFilesAndAFolder,
      { path: /^\/api\/nodes\/f1\/children/, body: page([]) },
    ])
    await screen.findByText('Reports')
    await select('Reports')

    const before = calls.length
    // `fireEvent`, not `userEvent`: this assertion is about what has happened by
    // the time the click handler returns, and `userEvent` awaits past it.
    fireEvent.click(within(bar()).getByRole('button', { name: 'Download' }))

    // Transient user activation is gone by the first await, and the paginated
    // walk is all awaits — so the picker has to be spent here or not at all,
    // with nothing awaited in front of it.
    expect(picker).toHaveBeenCalledTimes(1)
    expect(callsWhenOpened).toBe(before)
    // And the walk goes out in the same click rather than after the dialog is
    // answered: the two overlap, so the listing is done by the time a folder
    // has been chosen.
    expect(calls.slice(before).map((call) => call.url)).toEqual(['/api/nodes/f1/children'])
  })
})
