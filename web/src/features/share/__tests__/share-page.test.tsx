// @vitest-environment jsdom

/**
 * The recipient's page, through the real `<App />` — so the route table, its
 * params and what sits around it are the things under test. A `SharePage`
 * rendered on its own could never show that `/api/auth/me` is not asked, or
 * that `/s/:token` is not the catch-all's redirect to `/`.
 *
 * What matters most is what the page does not send: no session for a link
 * that is gated, spent or gone, and no custom header on the store's own URL.
 */

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useLocation } from 'react-router'

import type { ShareMeta } from '../../../lib/api'
import { renderApp, stubFetch, type StubbedCall, type StubRoute } from '../../../test/render'
import App from '../../../App'

const META = '/api/s/tok-1/meta'
const SESSION = '/api/s/tok-1/session'
const PASSWORD = '/api/s/tok-1/password'
const PREVIEW = '/api/s/tok-1/preview'

/** A fabricated store origin: presigned links point off this app, never at it. */
const STORE = 'https://objects.example.test'
const soon = () => new Date(Date.now() + 900_000).toISOString()

const meta = (over: Partial<ShareMeta> = {}): ShareMeta => ({
  name: 'shot.png',
  size: 2048,
  mime: 'image/png',
  requires_password: false,
  expires_at: null,
  exhausted: false,
  preview: true,
  ...over,
})

const live: StubRoute = { path: META, body: meta() }
const minted: StubRoute = { method: 'POST', path: SESSION, status: 204 }
const signed: StubRoute = { path: PREVIEW, body: { url: `${STORE}/shot.png`, expires_at: soon(), mime: 'image/png' } }

const UNAVAILABLE = "This link isn't available. It may have expired or been turned off."

interface StoreAnswer {
  body?: string
  status?: number
  headers?: Record<string, string>
}

/** Reports the query string, which is where `?reason=` has to disappear from. */
function Where() {
  return <span data-testid="where">{useLocation().search}</span>
}

/**
 * The API stub, the store's own leg, and one answer that changes: the first
 * `/preview` may be refused so the re-mint can be watched.
 */
function renderShare(
  routes: StubRoute[],
  { store = {}, route = '/s/tok-1', firstPreview }: { store?: Record<string, StoreAnswer>; route?: string; firstPreview?: { status: number; body: unknown } } = {},
) {
  const calls = stubFetch(routes)
  const api = globalThis.fetch as unknown as (url: string, init: RequestInit) => Promise<Response>
  let previews = 0
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const record = () =>
      calls.push({ method: init?.method ?? 'GET', url, headers: new Headers(init?.headers as HeadersInit | undefined), body: undefined })
    if (url === PREVIEW && firstPreview && previews++ === 0) {
      record()
      return new Response(JSON.stringify(firstPreview.body), {
        status: firstPreview.status,
        headers: { 'Content-Type': 'application/json' },
      })
    }
    const answer = store[url]
    if (!answer) return api(url, (init ?? {}) as RequestInit)
    record()
    return new Response(answer.body ?? '', { status: answer.status ?? 200, headers: answer.headers })
  })
  const rendered = renderApp(
    <>
      <App />
      <Where />
    </>,
    { route },
  )
  return { calls, ...rendered }
}

const sessions = (calls: StubbedCall[]) => calls.filter((c) => c.method === 'POST' && c.url === SESSION)
const asked = (calls: StubbedCall[], url: string) => calls.filter((c) => c.url === url)

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the six states', () => {
  it('says unavailable on a 404, and asks for nothing else', async () => {
    const { calls } = renderShare([{ path: META, status: 404, body: { code: 'not_found', message: 'no' } }])

    const notice = await screen.findByText(UNAVAILABLE)
    expect(notice.getAttribute('role')).toBe('status')
    expect(sessions(calls)).toHaveLength(0)
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull()
  })

  it("says Couldn't load on a 500, and Try again re-reads", async () => {
    const answer: StubRoute = { path: META, status: 500, body: { code: 'internal', message: 'fell over' } }
    renderShare([answer, minted, signed])

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain("Couldn't load this link.")
    expect(screen.queryByText(UNAVAILABLE)).toBeNull()

    answer.status = 200
    answer.body = meta()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))

    expect(await screen.findByRole('heading', { level: 1, name: 'shot.png' })).toBeTruthy()
  })

  it('names the network on a 429 rather than the link', async () => {
    renderShare([{ path: META, status: 429, body: { code: 'rate_limited', message: 'slow down' } }])

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain('Too many requests from your network — try again in a minute.')
    expect(screen.queryByText(UNAVAILABLE)).toBeNull()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
  })

  it('gates a password share, mints nothing on load, and opens on the right password', async () => {
    const { calls } = renderShare([
      { path: META, body: meta({ requires_password: true }) },
      { method: 'POST', path: PASSWORD, status: 401, body: { code: 'unauthorized', message: 'nope' } },
      signed,
    ])

    const field = await screen.findByLabelText('Password')
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull()
    expect(sessions(calls)).toHaveLength(0)

    await userEvent.type(field, 'wrong one')
    await userEvent.click(screen.getByRole('button', { name: 'Open' }))

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe("That password didn't work.")
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('')
    expect(asked(calls, PASSWORD)[0].body).toEqual({ password: 'wrong one' })

    // The right one.
    const routes = calls.length
    vi.unstubAllGlobals()
    const more = stubFetch([{ method: 'POST', path: PASSWORD, status: 204 }, signed])
    await userEvent.type(screen.getByLabelText('Password'), 'correct horse')
    await userEvent.click(screen.getByRole('button', { name: 'Open' }))

    expect(await screen.findByRole('heading', { level: 1, name: 'shot.png' })).toBeTruthy()
    expect(await screen.findByRole('img', { name: 'shot.png' })).toBeTruthy()
    expect(more.filter((c) => c.url === PASSWORD)).toHaveLength(1)
    // Never a guest session minted behind the gate's back.
    expect(sessions(calls).length + sessions(more).length).toBe(0)
    expect(routes).toBeGreaterThan(0)
  })

  it('says the limit is reached before it asks for a password, and sends nothing', async () => {
    const { calls } = renderShare([{ path: META, body: meta({ requires_password: true, exhausted: true }) }])

    const notice = await screen.findByText('This link has reached its download limit.')
    expect(notice.getAttribute('role')).toBe('status')
    // Spent beats gated: no credential is asked for that could not be used.
    expect(screen.queryByLabelText('Password')).toBeNull()
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull()
    expect(screen.queryByRole('img')).toBeNull()
    expect(sessions(calls)).toHaveLength(0)
    expect(asked(calls, PASSWORD)).toHaveLength(0)
    expect(asked(calls, PREVIEW)).toHaveLength(0)
  })
})

describe('the file', () => {
  it('mints one session, then shows the name, size and a plain Download anchor', async () => {
    const { calls } = renderShare([live, minted, signed])

    expect(await screen.findByRole('heading', { level: 1, name: 'shot.png' })).toBeTruthy()
    expect(screen.getByText('2.0 KB · image/png')).toBeTruthy()
    await waitFor(() => expect(sessions(calls)).toHaveLength(1))
    expect(sessions(calls)[0].headers.get('X-Drive-Client')).toBe('web')

    const link = screen.getByRole('link', { name: 'Download' })
    expect(link.tagName).toBe('A')
    expect(link.getAttribute('href')).toBe('/api/s/tok-1/download')
    // A `<Link to>` renders an `<a>` with this href too; react-router stamps
    // `data-discover` on every SPA link it draws, and a plain anchor never has
    // it. And the click: a routed link calls preventDefault on its way past,
    // which would swallow the navigation the 302 needs.
    expect(link.getAttribute('data-discover')).toBeNull()
    let prevented: boolean | undefined
    const watch = (e: Event) => {
      prevented = e.defaultPrevented
    }
    document.addEventListener('click', watch)
    try {
      await userEvent.click(link)
    } finally {
      document.removeEventListener('click', watch)
    }
    expect(prevented).toBe(false)
    expect(calls.some((c) => c.url.includes('/download'))).toBe(false)

    // Nobody asked who the visitor is: the route sits outside RequireAuth.
    expect(asked(calls, '/api/auth/me')).toHaveLength(0)
  })

  it('asks for the preview with the header, and reads the store bare', async () => {
    const { calls } = renderShare(
      [
        { path: META, body: meta({ name: 'notes.txt', mime: 'text/plain', size: 31 }) },
        minted,
        { path: PREVIEW, body: { url: `${STORE}/notes.txt`, expires_at: soon(), mime: 'text/plain' } },
      ],
      { store: { [`${STORE}/notes.txt`]: { body: 'the second line is the good one', headers: { 'Content-Length': '31' } } } },
    )

    expect(await screen.findByText('the second line is the good one')).toBeTruthy()
    // The link itself is an API call — a state-touching GET the server gates on
    // the header like a POST.
    const preview = asked(calls, PREVIEW)
    expect(preview).toHaveLength(1)
    expect(preview[0].headers.get('X-Drive-Client')).toBe('web')
    // The bytes are not: a custom header would make this cross-origin GET a
    // preflight the store answers with a 403.
    const read = asked(calls, `${STORE}/notes.txt`)
    expect(read).toHaveLength(1)
    expect(read[0].headers.get('X-Drive-Client')).toBeNull()
  })

  it('re-mints once on a 401 from the preview, and the image comes back', async () => {
    const { calls } = renderShare([live, minted, signed], {
      firstPreview: { status: 401, body: { code: 'unauthorized', message: 'no session' } },
    })

    // A 31-minute-old page: the session died, the link did not.
    const image = await screen.findByRole('img', { name: 'shot.png' })
    expect(image.getAttribute('src')).toBe(`${STORE}/shot.png`)
    expect(sessions(calls)).toHaveLength(2)
    expect(asked(calls, PREVIEW)).toHaveLength(2)
  })

  it('shows the card and the button for a PDF, with no preview asked for', async () => {
    const { calls } = renderShare([
      { path: META, body: meta({ name: 'report.pdf', mime: 'application/pdf', preview: false }) },
      minted,
    ])

    expect(await screen.findByRole('heading', { level: 1, name: 'report.pdf' })).toBeTruthy()
    await waitFor(() => expect(sessions(calls)).toHaveLength(1))
    expect(screen.getByRole('link', { name: 'Download' }).getAttribute('href')).toBe('/api/s/tok-1/download')
    expect(document.querySelector('iframe')).toBeNull()
    expect(asked(calls, PREVIEW)).toHaveLength(0)
  })
})

describe('coming back from a refused download', () => {
  it('shows the session line once and takes the reason out of the URL', async () => {
    const { calls } = renderShare([live, minted, signed], { route: '/s/tok-1?reason=session' })

    const line = await screen.findByText('Your session timed out — reopening.')
    expect(line.getAttribute('role')).toBe('status')
    await waitFor(() => expect(screen.getByTestId('where').textContent).not.toContain('reason='))
    // Reopening is a fresh mint, and the file is back.
    expect(await screen.findByRole('img', { name: 'shot.png' })).toBeTruthy()
    expect(sessions(calls)).toHaveLength(1)
  })
})
