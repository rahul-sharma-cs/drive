// @vitest-environment jsdom

import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useLocation, useParams } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubbedCall, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../../browser/FolderPage'

/**
 * The viewer, from the row that opens it.
 *
 * Two rules are load-bearing here and both are asserted rather than assumed.
 * The first is that the file's *name* is what opens it and nothing else on the
 * row does. The second is that the bytes never travel through this app: the
 * link comes from the API, and what reads it is an element's `src` or a bare
 * `fetch` — a `X-Drive-Client` header on that cross-origin GET would make it a
 * preflight the object store answers with a 403, which is a failure no test
 * against a stub would otherwise notice.
 */

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

const root = node({ id: 'root-1', parent_id: null, name: 'root' })

const shot = node({ id: 'f2', kind: 'file', name: 'shot.png', size: 2048, mime: 'image/png' })
const clip = node({ id: 'f3', kind: 'file', name: 'clip.mp4', size: 4096, mime: 'video/mp4' })
const reports = node({ id: 'f1', name: 'Reports' })

/** A fabricated store origin: presigned links point off this app, never at it. */
const STORE = 'https://objects.example.test'

const soon = () => new Date(Date.now() + 900_000).toISOString()

const listing = (items: DriveNode[], nextCursor: string | null = null): StubRoute[] => [
  { path: '/api/nodes/root-1', body: root },
  { path: '/api/nodes/root-1/children', body: { items, next_cursor: nextCursor } },
]

/** What the server answers for a type it will serve inline. */
const signed = (id: string, name: string, mime: string): StubRoute => ({
  path: `/api/files/${id}/preview`,
  body: { url: `${STORE}/${name}`, expires_at: soon(), mime },
})

/** Reports the query string, which is where the viewer's state actually lives. */
function Where() {
  return <span data-testid="where">{useLocation().search}</span>
}

function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
      <Where />
    </CurrentFolderProvider>
  )
}

interface StoreAnswer {
  /** A whole string, or a stream for an answer whose length is never declared. */
  body?: string | ReadableStream<Uint8Array>
  status?: number
  headers?: Record<string, string>
}

/**
 * The API stub, plus the object store's own leg.
 *
 * The store leg is separate because it answers with real response headers —
 * the text viewer reads `Content-Length` off one — and because it records what
 * headers the request carried, which is the whole point of the bare-fetch rule.
 */
function stubAll(routes: StubRoute[], store: Record<string, StoreAnswer> = {}): StubbedCall[] {
  const calls = stubFetch(routes)
  const api = globalThis.fetch as unknown as (url: string, init: RequestInit) => Promise<Response>
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const answer = store[url]
    if (!answer) return api(url, (init ?? {}) as RequestInit)
    calls.push({
      method: init?.method ?? 'GET',
      url,
      headers: new Headers(init?.headers as HeadersInit | undefined),
      body: undefined,
    })
    return new Response(answer.body ?? '', { status: answer.status ?? 200, headers: answer.headers })
  })
  return calls
}

function renderFolder(routes: StubRoute[], store: Record<string, StoreAnswer> = {}, route = '/') {
  const calls = stubAll(routes, store)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
        <Route path="/folders/:id" element={<FolderPage />} />
      </Route>
    </Routes>,
    { route, seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const where = () => screen.getByTestId('where').textContent
const rowFor = (name: string) =>
  screen.getAllByTestId('file-row').find((row) => within(row).queryByText(name))!
const previewCalls = (calls: StubbedCall[], id: string) =>
  calls.filter((call) => call.url === `/api/files/${id}/preview`)

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('opening a preview', () => {
  it('opens from the file name, and the rest of the row still only selects', async () => {
    const { calls } = renderFolder([...listing([reports, shot]), signed('f2', 'shot.png', 'image/png')])
    await screen.findByText('shot.png')

    // Everything that is not the name is selection, as it was before there was
    // anything to open.
    await userEvent.click(within(rowFor('shot.png')).getByText('2.0 KB'))
    expect(screen.getByRole('checkbox', { name: 'Select shot.png' }).getAttribute('aria-checked')).toBe('true')
    expect(where()).toBe('')
    expect(previewCalls(calls, 'f2')).toHaveLength(0)

    await userEvent.click(screen.getByRole('link', { name: 'shot.png' }))

    // A location, not a mode: the id is in the URL, so the viewer is linkable
    // and the back button closes it.
    await waitFor(() => expect(where()).toBe('?preview=f2'))
    const asked = previewCalls(calls, 'f2')
    expect(asked).toHaveLength(1)
    // The link itself is an API call like any other and carries the CSRF header.
    expect(asked[0].headers.get('X-Drive-Client')).toBe('web')

    const image = await screen.findByRole('img', { name: 'shot.png' })
    // Straight from the store. Not through this app, and not a blob: URL, which
    // would inherit this app's origin.
    expect(image.getAttribute('src')).toBe(`${STORE}/shot.png`)
  })

  it('walks into a folder from its name and never opens a viewer for one', async () => {
    const { calls } = renderFolder([
      ...listing([reports, shot]),
      { path: '/api/nodes/f1', body: reports },
      { path: '/api/nodes/f1/children', body: { items: [], next_cursor: null } },
    ])
    await screen.findByText('shot.png')

    await userEvent.click(screen.getByRole('link', { name: 'Reports' }))

    await waitFor(() => expect(screen.getByText('This folder is empty.')).toBeTruthy())
    // A folder is a place, and the viewer has nothing to show for one.
    expect(where()).toBe('')
    expect(calls.some((call) => call.url.includes('/preview'))).toBe(false)
  })

  it('opens the same viewer from the row menu', async () => {
    renderFolder([...listing([reports, shot]), signed('f2', 'shot.png', 'image/png')])
    await screen.findByText('shot.png')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for shot.png' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Preview' }))

    await waitFor(() => expect(where()).toBe('?preview=f2'))
    expect((await screen.findByRole('img', { name: 'shot.png' })).getAttribute('src')).toBe(
      `${STORE}/shot.png`,
    )
  })

  it('steps between files with the arrow keys, skipping the folders between them', async () => {
    renderFolder([
      ...listing([shot, reports, clip]),
      signed('f2', 'shot.png', 'image/png'),
      signed('f3', 'clip.mp4', 'video/mp4'),
    ])
    await screen.findByText('clip.mp4')

    await userEvent.click(screen.getByRole('link', { name: 'shot.png' }))
    expect(await screen.findByText('1 of 2')).toBeTruthy()

    await userEvent.keyboard('{ArrowRight}')

    // The folder sitting between them in the list is not a sibling: it is not
    // something the viewer can show, so stepping onto it would be a dead end.
    await waitFor(() => expect(where()).toBe('?preview=f3'))
    expect(await screen.findByText('2 of 2')).toBeTruthy()
    // And it is the second file that is on screen, not the folder.
    expect(within(screen.getByRole('dialog')).getByText('clip.mp4')).toBeTruthy()
  })

  it('closes on Escape and takes the parameter with it', async () => {
    renderFolder([...listing([shot]), signed('f2', 'shot.png', 'image/png')])
    await screen.findByText('shot.png')
    await userEvent.click(screen.getByRole('link', { name: 'shot.png' }))
    await screen.findByRole('dialog')

    await userEvent.keyboard('{Escape}')

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    // Opening pushed an entry, so closing goes back to the folder as it was —
    // no viewer, and no parameter left in the URL to reopen it on a reload.
    expect(where()).toBe('')
  })
})

describe('what the viewer shows', () => {
  it('offers the download when the server will not preview the type', async () => {
    renderFolder([
      ...listing([node({ id: 'f4', kind: 'file', name: 'diagram.svg', size: 900, mime: 'image/svg+xml' })]),
      { path: '/api/files/f4/preview', status: 415, body: { code: 'unsupported', message: 'no preview' } },
    ])
    await screen.findByText('diagram.svg')

    await userEvent.click(screen.getByRole('link', { name: 'diagram.svg' }))

    const card = await screen.findByTestId('no-preview')
    expect(within(card).getByText('No preview for this type')).toBeTruthy()
    // The one thing left worth offering, and it is the ordinary download route
    // — not the presigned inline link the server just refused to sign.
    expect(within(card).getByRole('link', { name: 'Download' }).getAttribute('href')).toBe(
      '/api/files/f4/download',
    )
    expect(screen.queryByRole('img', { name: 'diagram.svg' })).toBeNull()
  })

  it('reads a text file straight off the store, with no header on the request', async () => {
    const { calls } = renderFolder(
      [
        ...listing([node({ id: 'f5', kind: 'file', name: 'notes.txt', size: 12, mime: 'text/plain' })]),
        { path: '/api/files/f5/preview', body: { url: `${STORE}/notes.txt`, expires_at: soon(), mime: 'text/plain' } },
      ],
      { [`${STORE}/notes.txt`]: { body: 'the second line is the good one', headers: { 'Content-Length': '31' } } },
    )
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('link', { name: 'notes.txt' }))

    expect(await screen.findByText('the second line is the good one')).toBeTruthy()
    const read = calls.filter((call) => call.url === `${STORE}/notes.txt`)
    expect(read).toHaveLength(1)
    // A custom header would turn this plain cross-origin GET into a preflight,
    // and the store's rule answers preflights with a 403. Asked of the headers
    // the request really carried: a plain-object lookup reads a `Headers` as
    // nothing at all, so it would pass whether the rule held or not.
    expect(read[0].headers.get('X-Drive-Client')).toBeNull()
  })

  it('refuses a text file too big to read, and offers the download instead', async () => {
    renderFolder(
      [
        ...listing([node({ id: 'f5', kind: 'file', name: 'huge.log', size: 3_145_728, mime: 'text/plain' })]),
        { path: '/api/files/f5/preview', body: { url: `${STORE}/huge.log`, expires_at: soon(), mime: 'text/plain' } },
      ],
      // Three megabytes, declared before a byte of it is read.
      { [`${STORE}/huge.log`]: { body: 'a line of it', headers: { 'Content-Length': '3145728' } } },
    )
    await screen.findByText('huge.log')

    await userEvent.click(screen.getByRole('link', { name: 'huge.log' }))

    expect(await screen.findByTestId('no-preview')).toBeTruthy()
    expect(screen.queryByText('a line of it')).toBeNull()
  })

  it('stops reading a text file whose length the store never declared', async () => {
    // A chunked answer carries no Content-Length at all, and `Number(null)` is
    // zero — under every ceiling there is.
    const CHUNK = 256 * 1024
    const CHUNKS = 12
    let sent = 0
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (sent === CHUNKS) {
          controller.close()
          return
        }
        sent += 1
        controller.enqueue(new Uint8Array(CHUNK).fill(0x61))
      },
    })

    renderFolder(
      [
        ...listing([node({ id: 'f5', kind: 'file', name: 'chunked.log', size: 3_145_728, mime: 'text/plain' })]),
        { path: '/api/files/f5/preview', body: { url: `${STORE}/chunked.log`, expires_at: soon(), mime: 'text/plain' } },
      ],
      { [`${STORE}/chunked.log`]: { body } },
    )
    await screen.findByText('chunked.log')

    await userEvent.click(screen.getByRole('link', { name: 'chunked.log' }))

    expect(await screen.findByTestId('no-preview')).toBeTruthy()
    // The card is not the point — a read that swallowed all three megabytes and
    // measured them afterwards shows the same one. The point is that the store
    // stopped being asked: the transfer was cut off part way down the file.
    expect(sent).toBeLessThan(CHUNKS)
  })

  it('will not guess a PDF from a name the link carries no type for', async () => {
    renderFolder([
      ...listing([node({ id: 'f7', kind: 'file', name: 'invoice.pdf', size: 5000, mime: 'application/pdf' })]),
      // Signed, but with no type named on it. The stored mime is the client's
      // own claim and the name is worth even less — and `pdf` is the one body
      // that is a frame the browser navigates to, so a guess here is a guess
      // about what gets loaded, not about which element shows it.
      { path: '/api/files/f7/preview', body: { url: `${STORE}/invoice.pdf`, expires_at: soon(), mime: '' } },
    ])
    await screen.findByText('invoice.pdf')

    await userEvent.click(screen.getByRole('link', { name: 'invoice.pdf' }))

    expect(await screen.findByTestId('no-preview')).toBeTruthy()
    expect(document.querySelector('iframe')).toBeNull()
  })

  it('shows the download card for a PDF on a touch device rather than an empty frame', async () => {
    // iOS Safari renders a framed PDF as a blank box; a coarse primary pointer
    // is what stands in for "this browser will not show it".
    vi.stubGlobal(
      'matchMedia',
      (query: string) => ({ matches: query.includes('coarse'), media: query }) as MediaQueryList,
    )
    renderFolder([
      ...listing([node({ id: 'f6', kind: 'file', name: 'report.pdf', size: 5000, mime: 'application/pdf' })]),
      { path: '/api/files/f6/preview', body: { url: `${STORE}/report.pdf`, expires_at: soon(), mime: 'application/pdf' } },
    ])
    await screen.findByText('report.pdf')

    await userEvent.click(screen.getByRole('link', { name: 'report.pdf' }))

    expect(await screen.findByTestId('no-preview')).toBeTruthy()
    expect(document.querySelector('iframe')).toBeNull()
  })
})

/**
 * Opens the deep link on a fake clock, with every signature carrying the same
 * `expiresAt`, and counts how many times the app went and asked for one.
 *
 * The count is the whole instrument here: each new signature re-points the
 * `<img>`'s `src`, so a timer that fires more than it should is not a wasted
 * API call — it is the file coming back off the store, again, for as long as
 * the viewer is open.
 */
function openWithExpiry(expiresAt: string) {
  let signings = 0
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  vi.stubGlobal('fetch', async (url: string) => {
    if (url === '/api/nodes/root-1') return json(root)
    if (url === '/api/nodes/root-1/children') return json({ items: [shot], next_cursor: 'page-2' })
    if (url === '/api/files/f2/preview') {
      signings += 1
      return json({ url: `${STORE}/shot.png?sig=${signings}`, expires_at: expiresAt, mime: 'image/png' })
    }
    throw new Error(`unstubbed request: ${url}`)
  })

  // Straight in on the parameter: this is the deep link, and it needs no
  // click to open — which keeps the fake clock out of userEvent's way.
  renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
      </Route>
    </Routes>,
    { route: '/?preview=f2', seed: (client) => client.setQueryData(meKey, user) },
  )

  // Testing Library's own waiting helpers look for Jest to decide whether the
  // clock is faked, find nothing here, and would sit on a timer that never
  // advances — so the settling is done by hand.
  const settle = async (ms = 0) => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  return { signings: () => signings, settle }
}

describe('a preview parameter the list cannot account for', () => {
  it('opens on a deep link to a file the loaded pages do not hold', async () => {
    renderFolder(
      [
        ...listing([shot]),
        {
          path: '/api/files/f9/preview',
          body: { url: `${STORE}/elsewhere.png`, expires_at: soon(), mime: 'image/png' },
        },
      ],
      {},
      '/?preview=f9',
    )

    const dialog = await screen.findByRole('dialog')
    // The answer carries a URL, an expiry and a type, and no name — so the
    // title is the generic one, and the counter, which counts loaded siblings,
    // has nothing to count rather than counting wrong.
    expect(within(dialog).getByText('Preview')).toBeTruthy()
    expect(within(dialog).queryByText(/ of /)).toBeNull()
    await waitFor(() =>
      expect(dialog.querySelector('img')?.getAttribute('src')).toBe(`${STORE}/elsewhere.png`),
    )
  })

  it('closes without throwing on a preview id that is not a selector', async () => {
    // The id comes out of the address bar, and on the way out the viewer looks
    // for the row it was opened from by attribute. A quote in it ends the
    // attribute selector early and `querySelector` throws — out of a focus
    // handler, where nothing catches it.
    const crafted = 'a"]'
    const errors: unknown[] = []
    const record = (event: ErrorEvent) => errors.push(event.error)
    window.addEventListener('error', record)
    try {
      renderFolder(
        [
          ...listing([shot]),
          {
            path: `/api/files/${crafted}/preview`,
            status: 415,
            body: { code: 'unsupported', message: 'no preview' },
          },
        ],
        {},
        `/?preview=${encodeURIComponent(crafted)}`,
      )

      await screen.findByRole('dialog')
      await userEvent.keyboard('{Escape}')

      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
      expect(errors).toEqual([])
    } finally {
      window.removeEventListener('error', record)
    }
  })
})

describe('a link that is about to expire', () => {
  it('is replaced while the viewer is still open', async () => {
    vi.useFakeTimers()
    const { signings, settle } = openWithExpiry(new Date(Date.now() + 900_000).toISOString())

    await settle()
    expect(screen.getByRole('img', { name: 'shot.png' }).getAttribute('src')).toBe(
      `${STORE}/shot.png?sig=1`,
    )
    // The folder has another page, so the count says what it is counting.
    expect(screen.getByText('1 of 1 loaded')).toBeTruthy()

    // A minute before the link dies. Nothing else in this app would ever go and
    // get another one: refetch-on-focus is off, and an expired link inside an
    // <img> or an <iframe> is a 403 nobody reports.
    await settle(840_001)

    expect(signings()).toBe(2)
    expect(screen.getByRole('img', { name: 'shot.png' }).getAttribute('src')).toBe(
      `${STORE}/shot.png?sig=2`,
    )
  })

  it('does not chase an expiry that has already passed', async () => {
    vi.useFakeTimers()
    // A browser clock running ten seconds ahead of the signer's is enough:
    // subtract the refresh margin and the delay is negative, which a timer
    // reads as "now". The refetch that follows re-arms it, and the loop that
    // makes is one download of the file per round trip.
    const { signings, settle } = openWithExpiry(new Date(Date.now() - 10_000).toISOString())

    await settle()
    expect(screen.getByRole('img', { name: 'shot.png' })).toBeTruthy()

    await settle(10_000)

    // The one the deep link opened with, and nothing since.
    expect(signings()).toBe(1)
  })

  it('does not arm a timer for an expiry further out than a timer can hold', async () => {
    vi.useFakeTimers()
    // `setTimeout` truncates its delay to a signed 32-bit int, so anything past
    // about 24.8 days fires on the next tick instead of in a month — the same
    // loop, reached from the opposite end.
    const { signings, settle } = openWithExpiry(new Date(Date.now() + 40 * 86_400_000).toISOString())

    await settle()
    expect(screen.getByRole('img', { name: 'shot.png' })).toBeTruthy()

    await settle(1_000)

    expect(signings()).toBe(1)
  })
})
