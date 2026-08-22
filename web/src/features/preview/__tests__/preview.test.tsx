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
  body?: string
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
      headers: (init?.headers ?? {}) as Record<string, string>,
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
    expect(asked[0].headers['X-Drive-Client']).toBe('web')

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
    // and the store's rule answers preflights with a 403.
    expect(read[0].headers['X-Drive-Client']).toBeUndefined()
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

describe('a link that is about to expire', () => {
  it('is replaced while the viewer is still open', async () => {
    vi.useFakeTimers()
    const expiresAt = new Date(Date.now() + 900_000).toISOString()
    let signed = 0
    const json = (body: unknown, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    vi.stubGlobal('fetch', async (url: string) => {
      if (url === '/api/nodes/root-1') return json(root)
      if (url === '/api/nodes/root-1/children') return json({ items: [shot], next_cursor: 'page-2' })
      if (url === '/api/files/f2/preview') {
        signed += 1
        return json({ url: `${STORE}/shot.png?sig=${signed}`, expires_at: expiresAt, mime: 'image/png' })
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

    expect(signed).toBe(2)
    expect(screen.getByRole('img', { name: 'shot.png' }).getAttribute('src')).toBe(
      `${STORE}/shot.png?sig=2`,
    )
  })
})
