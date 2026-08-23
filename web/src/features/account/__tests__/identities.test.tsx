// @vitest-environment jsdom

/**
 * The account screen's sign-in methods, and what the password section becomes
 * for an account that has no password.
 *
 * `has_password: false` is not a hypothetical state: linking Google to an
 * account that had signed up but never verified clears the password it was
 * created with, so it is what the account screen sees after exactly the flow
 * this phase adds.
 */

import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { meKey } from '../../auth/session'
import { AccountPage } from '../AccountPage'

const user: Me = {
  id: 'u-1',
  email: 'ada@example.test',
  display_name: 'Ada Lovelace',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
  has_password: true,
}

const identity = {
  id: 'id-1',
  provider: 'google' as const,
  email_at_link: 'ada@example.test',
  created_at: '2026-08-01T00:00:00Z',
  last_login_at: '2026-08-20T00:00:00Z',
}

const linked: StubRoute = {
  path: '/api/auth/identities',
  body: { items: [identity], next_cursor: null },
}

function renderAccount({ me = user, routes = [] }: { me?: Me; routes?: StubRoute[] } = {}) {
  const calls = stubFetch([
    { path: '/api/auth/sessions', body: { items: [], next_cursor: null } },
    ...routes,
  ])
  const rendered = renderApp(<AccountPage />, {
    route: '/account',
    seed: (client) => client.setQueryData(meKey, me),
  })
  return { calls, ...rendered }
}

const methods = () => screen.getByRole('region', { name: 'Sign-in methods' })

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the linked methods', () => {
  it('lists the identity with its address and when it was linked', async () => {
    renderAccount({ routes: [linked] })

    const row = await within(methods()).findByRole('listitem')
    expect(within(row).getByText(/Google · ada@example\.test/)).toBeTruthy()
    expect(within(row).getByText(/linked .+ · last used /)).toBeTruthy()
  })

  it('says so plainly when nothing is linked', async () => {
    renderAccount()

    expect(
      await within(methods()).findByText(/Nothing linked\./),
    ).toBeTruthy()
  })
})

describe('unlinking', () => {
  it('is live for an account that also has a password', async () => {
    renderAccount({ routes: [linked] })

    await waitFor(() =>
      expect((within(methods()).getByRole('button', { name: 'Unlink' }) as HTMLButtonElement).disabled).toBe(false),
    )
    expect(
      within(methods()).queryByText(/This is the only way into your account\./),
    ).toBeNull()
  })

  it('is disabled with the reason when it is the only way in', async () => {
    renderAccount({ me: { ...user, has_password: false }, routes: [linked] })

    await waitFor(() =>
      expect((within(methods()).getByRole('button', { name: 'Unlink' }) as HTMLButtonElement).disabled).toBe(true),
    )
    expect(
      within(methods()).getByText(/This is the only way into your account\./),
    ).toBeTruthy()
  })

  it('sends the DELETE with the CSRF header and drops the row', async () => {
    const { calls } = renderAccount({
      routes: [linked, { method: 'DELETE', path: '/api/auth/identities/id-1', status: 204 }],
    })

    await userEvent.click(await within(methods()).findByRole('button', { name: 'Unlink' }))

    await waitFor(() => expect(within(methods()).queryByRole('listitem')).toBeNull())
    const sent = calls.find((c) => c.method === 'DELETE')
    expect(sent?.url).toBe('/api/auth/identities/id-1')
    expect(sent?.headers.get('X-Drive-Client')).toBe('web')
  })

  it('shows the server’s refusal and keeps the row on a 409', async () => {
    renderAccount({
      routes: [
        linked,
        {
          method: 'DELETE',
          path: '/api/auth/identities/id-1',
          status: 409,
          body: { code: 'unsupported', message: 'That is the only way to sign in to this account.' },
        },
      ],
    })

    await userEvent.click(await within(methods()).findByRole('button', { name: 'Unlink' }))

    expect(
      await screen.findByText('That is the only way to sign in to this account.'),
    ).toBeTruthy()
    expect(within(methods()).getByRole('listitem')).toBeTruthy()
  })
})

describe('the password section splits on has_password', () => {
  it('is the change form for an account with a password', async () => {
    renderAccount()

    expect(await screen.findByRole('button', { name: 'Change password' })).toBeTruthy()
    expect(screen.queryByText(/You sign in with Google\./)).toBeNull()
  })

  it('points a password-less account at the reset flow instead of a form it cannot fill', async () => {
    renderAccount({ me: { ...user, has_password: false }, routes: [linked] })

    expect(await screen.findByText(/You sign in with Google\./)).toBeTruthy()
    // No current password exists, so there is nothing here to ask for one.
    expect(screen.queryByRole('button', { name: 'Change password' })).toBeNull()
    expect(screen.queryByLabelText('Current password')).toBeNull()
  })
})
