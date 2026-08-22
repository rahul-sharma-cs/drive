// @vitest-environment jsdom

import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Route, Routes } from 'react-router'

import { Sheet } from '@/components/ui/sheet'

import type { AuthSession, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { TopBar } from '../../../app/TopBar'
import { meKey } from '../../auth/session'
import { AccountPage } from '../AccountPage'

const user: Me = {
  id: 'u1',
  email: 'someone@example.test',
  display_name: 'Ada Lovelace',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
}

/** Two live sign-ins: the one reading this page, and one somewhere else. */
const sessions: AuthSession[] = [
  {
    id: 'sess-here',
    created_at: '2026-08-20T09:00:00Z',
    last_seen_at: '2026-08-22T08:00:00Z',
    ip: '192.0.2.10',
    user_agent:
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
    current: true,
  },
  {
    id: 'sess-elsewhere',
    created_at: '2026-08-18T09:00:00Z',
    last_seen_at: null,
    ip: '198.51.100.7',
    user_agent:
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36 Edg/139.0.0.0',
    current: false,
  },
]

/**
 * The account screen under the real top bar, because half of what this file
 * asserts is that a change made down here reaches the avatar up there. The
 * layout itself is out: it would bring the upload engine (workers, IndexedDB)
 * with it, and none of that is what these tests are about.
 *
 * The bare `<Sheet>` stands in for the one the layout wraps its chrome in. The
 * top bar's hamburger is that drawer's `SheetTrigger`, and a Radix trigger
 * rendered outside its root throws rather than degrading — so composing the bar
 * on its own means supplying the root it expects. No panel: nothing here opens
 * the drawer, and the rail has a test file of its own.
 */
function renderAccount(routes: StubRoute[] = []) {
  const calls = stubFetch([
    { path: '/api/auth/sessions', body: { items: sessions, next_cursor: null } },
    ...routes,
  ])
  const rendered = renderApp(
    <Routes>
      <Route
        path="/account"
        element={
          <Sheet>
            <TopBar />
            <AccountPage />
          </Sheet>
        }
      />
      <Route path="/login" element={<p>login</p>} />
    </Routes>,
    { route: '/account', seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const face = () => screen.getByRole('button', { name: 'Your account' })

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('getting here', () => {
  it('hangs off the account menu, above sign out', async () => {
    renderAccount()

    await userEvent.click(face())
    const menu = await screen.findByRole('menu')
    const settings = within(menu).getByRole('menuitem', { name: 'Account settings' })
    expect(settings.getAttribute('href')).toBe('/account')
    // Order is the point: the destination first, the exit last.
    expect(settings.compareDocumentPosition(within(menu).getByRole('menuitem', { name: 'Sign out' }))).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    )
  })
})

describe('the profile section', () => {
  it('saves the display name and repaints the avatar from the answer, not a refetch', async () => {
    const { calls } = renderAccount([
      { method: 'PATCH', path: '/api/auth/me', body: { ...user, display_name: 'Grace Hopper' } },
    ])

    expect(within(face()).getByText('AL')).toBeTruthy()

    await userEvent.clear(screen.getByLabelText('Display name'))
    await userEvent.type(screen.getByLabelText('Display name'), 'Grace Hopper')
    await userEvent.click(screen.getByRole('button', { name: 'Save name' }))

    // The initials come from the `me` cache the top bar reads, so this is the
    // assertion that the PATCH's own answer was written into it.
    await waitFor(() => expect(within(face()).getByText('GH')).toBeTruthy())

    const patch = calls.find((c) => c.method === 'PATCH')
    expect(patch?.url).toBe('/api/auth/me')
    expect(patch?.body).toEqual({ display_name: 'Grace Hopper' })
    expect(patch?.headers['X-Drive-Client']).toBe('web')
    // And that it did so without spending a request on an answer it already had.
    expect(calls.filter((c) => c.method === 'GET' && c.url === '/api/auth/me')).toHaveLength(0)
  })

  it('shows the address it cannot change', async () => {
    renderAccount()
    const email = screen.getByLabelText('Email') as HTMLInputElement
    expect(email.value).toBe('someone@example.test')
    expect(email.readOnly).toBe(true)
  })
})

describe('the password section', () => {
  it('never sends a mistyped confirmation, and shows the server’s refusal when it does send', async () => {
    const { calls } = renderAccount([
      {
        method: 'POST',
        path: '/api/auth/password',
        status: 401,
        body: { code: 'unauthorized', message: 'that password is not right' },
      },
    ])

    await userEvent.type(screen.getByLabelText('Current password'), 'old-passphrase')
    await userEvent.type(screen.getByLabelText('New password'), 'new-passphrase-a')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'new-passphrase-b')
    await userEvent.click(screen.getByRole('button', { name: 'Change password' }))

    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Those two passwords don’t match.')
    // A typo in the repeat is not the server's business — and the endpoint it
    // would have hit is the rate-limited one that reaches Argon2.
    expect(calls.filter((c) => c.url === '/api/auth/password')).toHaveLength(0)

    await userEvent.clear(screen.getByLabelText('Confirm new password'))
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'new-passphrase-a')
    await userEvent.click(screen.getByRole('button', { name: 'Change password' }))

    await waitFor(() => expect(calls.filter((c) => c.url === '/api/auth/password')).toHaveLength(1))
    expect(calls.find((c) => c.url === '/api/auth/password')?.body).toEqual({
      current_password: 'old-passphrase',
      new_password: 'new-passphrase-a',
    })
    // The server writes this copy; restating it here would only make it vaguer.
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'that password is not right')
  })
})

describe('the sessions section', () => {
  it('revokes one device, keeps the rest, and never offers to revoke this one', async () => {
    const { calls } = renderAccount([
      { method: 'DELETE', path: '/api/auth/sessions/sess-elsewhere', status: 204 },
    ])

    expect(await screen.findByText('Edge on Windows')).toBeTruthy()
    expect(screen.getByText('Chrome on macOS')).toBeTruthy()
    expect(screen.getByText('This device')).toBeTruthy()

    // Exactly one Revoke on a two-row list: an affordance that signs you out of
    // the screen you are standing on is a trap.
    expect(screen.getAllByRole('button', { name: /^Revoke/ })).toHaveLength(1)

    await userEvent.click(screen.getByRole('button', { name: 'Revoke Edge on Windows' }))

    await waitFor(() => expect(screen.queryByText('Edge on Windows')).toBeNull())
    expect(screen.getByText('Chrome on macOS')).toBeTruthy()
    const del = calls.find((c) => c.method === 'DELETE')
    expect(del?.url).toBe('/api/auth/sessions/sess-elsewhere')
    expect(del?.headers['X-Drive-Client']).toBe('web')
  })

  it('signs out everywhere behind a confirmation, and leaves for the sign-in screen', async () => {
    const { calls, client } = renderAccount([
      { method: 'POST', path: '/api/auth/logout-all', status: 204 },
    ])

    await userEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }))
    const dialog = await screen.findByRole('dialog')
    // Nothing has happened yet — the button opens a question, not the action.
    expect(calls.filter((c) => c.url === '/api/auth/logout-all')).toHaveLength(0)

    await userEvent.click(within(dialog).getByRole('button', { name: 'Sign out everywhere' }))

    await waitFor(() => expect(calls.find((c) => c.url === '/api/auth/logout-all')?.method).toBe('POST'))
    expect(await screen.findByText('login')).toBeTruthy()
    // This browser's cookie is gone too, so the cached account has to go with it.
    expect(client.getQueryData(meKey)).toBeNull()
  })
})
