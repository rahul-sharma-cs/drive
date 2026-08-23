// @vitest-environment jsdom

/**
 * The Google affordance on the two signed-out screens.
 *
 * The load-bearing assertion in this file is the negative one: pressing
 * "Continue with Google" must make no request. It is a top-level navigation to
 * a server route that answers with a 302 to Google — a `<Link>` would be
 * swallowed by the router, and `lib/api.ts` would turn it into an XHR carrying
 * `X-Drive-Client`. Neither can complete an authorization flow, and both look
 * exactly like the working version until somebody presses the button.
 */

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { meKey } from '../session'
import { LoginPage } from '../LoginPage'
import { SignupPage } from '../SignupPage'

const configured: StubRoute = { path: '/api/auth/providers', body: { google: true } }

afterEach(() => {
  vi.unstubAllGlobals()
})

const googleLink = () => screen.findByRole('link', { name: 'Continue with Google' })

describe('the button appears only where the deployment offers it', () => {
  it('renders on the sign-in screen when the server says google', async () => {
    stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login' })

    expect((await googleLink()).getAttribute('href')).toBe('/api/auth/google/start')
  })

  it('renders on the sign-up screen when the server says google', async () => {
    stubFetch([configured])
    renderApp(<SignupPage />, { route: '/signup' })

    expect((await googleLink()).getAttribute('href')).toBe('/api/auth/google/start')
  })

  it('renders on neither screen when the server says it is not configured', async () => {
    // `stubFetch`'s own default is `{google: false}` — the shape a clone with no
    // Google client answers with.
    stubFetch([])
    const { unmount } = renderApp(<LoginPage />, { route: '/login' })

    // The form is up, so the providers answer has been and gone.
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'Continue with Google' })).toBeNull()

    unmount()
    renderApp(<SignupPage />, { route: '/signup' })
    expect(await screen.findByRole('button', { name: 'Create account' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'Continue with Google' })).toBeNull()
  })
})

describe('pressing it is a navigation, not a request', () => {
  it('is an anchor to the server route and fetches nothing when clicked', async () => {
    const calls = stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login' })

    const link = await googleLink()
    expect(link.tagName).toBe('A')
    // jsdom refuses the navigation itself ("Not implemented: navigation to
    // another Document"), which is exactly the proof wanted: the click left the
    // document rather than being handled inside it.
    await userEvent.click(link)

    expect(calls.filter((c) => c.url.includes('/auth/google'))).toEqual([])
  })
})

describe('what a round trip that came back with nothing says', () => {
  it('shows the generic line for ?error=google and not the closed one', async () => {
    stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login?error=google' })

    expect(
      await screen.findByText("Google sign-in didn't complete. Try again, or use your email and password."),
    ).toBeTruthy()
    expect(screen.queryByText('This Drive is not accepting new accounts.')).toBeNull()
  })

  it('shows the closed line for ?error=google_closed and not the generic one', async () => {
    stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login?error=google_closed' })

    expect(await screen.findByText('This Drive is not accepting new accounts.')).toBeTruthy()
    expect(
      screen.queryByText("Google sign-in didn't complete. Try again, or use your email and password."),
    ).toBeNull()
  })

  it('says nothing at all without the parameter', async () => {
    stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login' })

    expect(await googleLink()).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('takes the parameter out of the URL when dismissed, so a reload is not shown it again', async () => {
    stubFetch([configured])
    renderApp(<LoginPage />, { route: '/login?error=google' })

    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }))

    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(window.location.search).not.toContain('error=google')
  })
})

describe('what the login answer carries into the session cache', () => {
  it('keeps has_password from the login body rather than dropping it', async () => {
    const user = {
      id: 'u-1',
      email: 'ada@example.test',
      display_name: 'Ada Lovelace',
      root_id: 'root-1',
      email_verified_at: '2026-08-17T00:00:00Z',
      has_password: true,
    }
    stubFetch([configured, { method: 'POST', path: '/api/auth/login', body: user }])
    const { client } = renderApp(<LoginPage />, { route: '/login' })

    await userEvent.type(screen.getByLabelText('Email'), 'ada@example.test')
    await userEvent.type(screen.getByLabelText('Password'), 'correct horse battery')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    // `me` is seeded from the login body and kept forever (`staleTime:
    // Infinity`), so a field dropped here is a field the account screen never
    // sees — and `has_password: false` on a password account would show it the
    // Google body.
    await waitFor(() => expect(client.getQueryData(meKey)).toEqual(user))
  })
})
