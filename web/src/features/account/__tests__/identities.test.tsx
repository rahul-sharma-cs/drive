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
import { Toaster } from 'sonner'
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

/**
 * The `<Toaster>` is the one from `main.tsx`: what the screen says back after an
 * unlink is a toast, and without a toaster mounted `toast.success(...)` is a
 * call into a store that renders nowhere — which a test asserting the call,
 * rather than the sentence, would never notice.
 */
function renderAccount({
  me = user,
  routes = [],
  google = true,
}: { me?: Me; routes?: StubRoute[]; google?: boolean } = {}) {
  const calls = stubFetch([
    ...routes,
    { path: '/api/auth/sessions', body: { items: [], next_cursor: null } },
    { path: '/api/auth/providers', body: { google } },
  ])
  const rendered = renderApp(
    <>
      <AccountPage />
      <Toaster />
    </>,
    {
      route: '/account',
      seed: (client) => client.setQueryData(meKey, me),
    },
  )
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

describe('a deployment with no provider configured', () => {
  it('goes back to the account screen it had before this feature existed', async () => {
    renderAccount({ google: false })

    // Drawn while the answer is outstanding: there may be something to show.
    expect(methods()).toBeTruthy()
    // And gone once there is not. A clone with no Google client would otherwise
    // keep a heading, a rule and the permanently dead line under them.
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Sign-in methods' })).toBeNull())
  })

  it('keeps the section for anything already linked', async () => {
    renderAccount({ google: false, routes: [linked] })

    // Unconfiguring the provider does not unlink what it linked, and a row the
    // person cannot see is a row they cannot take away.
    const row = await within(methods()).findByRole('listitem')
    expect(within(row).getByText(/Google · ada@example\.test/)).toBeTruthy()
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

    const button = await waitFor(() => {
      const found = within(methods()).getByRole('button', { name: 'Unlink' }) as HTMLButtonElement
      expect(found.disabled).toBe(true)
      return found
    })
    const reason = within(methods()).getByText(/This is the only way into your account\./)
    // `disabled` takes the button out of the tab order, so a keyboard user
    // never lands on it and never hears why. The reason has to belong to the
    // control, not merely sit near it.
    expect(button.getAttribute('aria-describedby')).toBe(reason.id)
    expect(reason.id).not.toBe('')
  })

  it('asks first, and asks the server nothing until the answer is yes', async () => {
    const { calls } = renderAccount({
      routes: [linked, { method: 'DELETE', path: '/api/auth/identities/id-1', status: 204 }],
    })

    await userEvent.click(await within(methods()).findByRole('button', { name: 'Unlink' }))

    // The row's Unlink sits where the session list's Revoke sits, in the same
    // variant and size, and there is no undo behind it.
    const asking = await screen.findByRole('dialog')
    expect(within(asking).getByRole('heading', { name: 'Unlink Google?' })).toBeTruthy()
    expect(within(asking).getByText(/ada@example\.test/)).toBeTruthy()
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)

    await userEvent.click(within(asking).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)
    expect(within(methods()).getByRole('listitem')).toBeTruthy()
  })

  it('sends the DELETE with the CSRF header and drops the row', async () => {
    const { calls } = renderAccount({
      routes: [linked, { method: 'DELETE', path: '/api/auth/identities/id-1', status: 204 }],
    })

    await userEvent.click(await within(methods()).findByRole('button', { name: 'Unlink' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Unlink' }))

    await waitFor(() => expect(within(methods()).queryByRole('listitem')).toBeNull())
    expect(await screen.findByText('Sign-in method removed')).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    const sent = calls.find((c) => c.method === 'DELETE')
    expect(sent?.url).toBe('/api/auth/identities/id-1')
    expect(sent?.headers.get('X-Drive-Client')).toBe('web')
  })

  it('drops a row the server no longer has a link for', async () => {
    renderAccount({
      routes: [
        linked,
        {
          method: 'DELETE',
          path: '/api/auth/identities/id-1',
          status: 404,
          body: { code: 'not_found', message: 'no such identity' },
        },
      ],
    })

    await userEvent.click(await within(methods()).findByRole('button', { name: 'Unlink' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Unlink' }))

    // The link went between this list being fetched and the click — another
    // tab, another device. The row describes something that is already gone,
    // and leaving it on screen with a live Unlink is the one outcome that is
    // wrong whichever way it is read.
    await waitFor(() => expect(within(methods()).queryByRole('listitem')).toBeNull())
    expect(await screen.findByText('That sign-in method was already removed')).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
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
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Unlink' }))

    expect(
      await screen.findByText('That is the only way to sign in to this account.'),
    ).toBeTruthy()
    // By role the page is hidden behind the open dialog, so the row is looked
    // for by its text: it is still there, which is the half of the refusal the
    // wording is about.
    expect(screen.getByText(/Google · ada@example\.test/)).toBeTruthy()
    // Still asking: the refusal is about the row the question names, and
    // closing on it would put the answer behind the screen it was asked from.
    expect(screen.getByRole('dialog')).toBeTruthy()
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
