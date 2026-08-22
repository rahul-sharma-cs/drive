// @vitest-environment jsdom

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { renderApp, stubFetch } from '../../../test/render'
import { ForgotPage } from '../ForgotPage'
import { LoginPage } from '../LoginPage'
import { ResetPage } from '../ResetPage'
import { SignupPage } from '../SignupPage'
import { VerifyPage } from '../VerifyPage'
import { meKey } from '../session'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('/verify', () => {
  it('redeems the token from the query string and says so', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/verify-email', body: { status: 'ok' } }])
    renderApp(<VerifyPage />, { route: '/verify?token=tok-123' })

    expect(await screen.findByText(/your email is verified/i)).toBeTruthy()
    expect(calls).toHaveLength(1)
    expect(calls[0].method).toBe('POST')
    expect(calls[0].url).toBe('/api/auth/verify-email')
    expect(calls[0].body).toEqual({ token: 'tok-123' })
    // The CSRF gate rejects a cookie-authed mutation without this header.
    expect(calls[0].headers['X-Drive-Client']).toBe('web')
  })

  it("shows the server's own message when the link is spent", async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/verify-email',
        status: 422,
        body: { code: 'invalid', message: 'this verification link is invalid or has expired' },
      },
    ])
    renderApp(<VerifyPage />, { route: '/verify?token=old' })

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'this verification link is invalid or has expired',
    )
  })

  it('never posts when the link carries no token', async () => {
    const calls = stubFetch([])
    renderApp(<VerifyPage />, { route: '/verify' })

    expect(await screen.findByText(/missing its token/i)).toBeTruthy()
    expect(calls).toHaveLength(0)
  })
})

describe('/login', () => {
  it('signs in and primes the session cache from the response', async () => {
    const user = {
      id: 'u1',
      email: 'someone@example.test',
      display_name: 'Someone',
      root_id: 'r1',
      email_verified_at: '2026-08-17T00:00:00Z',
    }
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/login', body: user }])
    const { client } = renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    await waitFor(() => expect(client.getQueryData(meKey)).toEqual(user))
    // One request: login answers with the whole `me` shape, so a second
    // /auth/me would only ask for what this one already returned.
    expect(calls).toHaveLength(1)
    expect(calls[0].body).toEqual({ email: 'someone@example.test', password: 'hunter2hunter2' })
  })

  it('reports a refused sign-in without touching the session', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/login',
        status: 401,
        body: { code: 'unauthorized', message: 'that email and password combination is not right' },
      },
    ])
    const { client } = renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'that email and password combination is not right',
    )
    expect(client.getQueryData(meKey)).toBeUndefined()
  })
})

describe('/signup', () => {
  it('claims nothing about whether the address was free', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/signup', body: { status: 'ok' } }])
    renderApp(<SignupPage />)

    await userEvent.type(screen.getByLabelText(/name/i), 'Someone')
    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /create account/i }))

    // The server's answer is identical for a free and a taken address, so the
    // page must not say an account was created.
    expect(await screen.findByText(/is new here/i)).toBeTruthy()
    expect(screen.queryByText(/account created/i)).toBeNull()
    expect(calls[0].body).toEqual({
      email: 'someone@example.test',
      password: 'hunter2hunter2',
      display_name: 'Someone',
    })
  })

  it('shows the refusal when signups are closed', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/signup',
        status: 403,
        body: { code: 'unsupported', message: 'Drive is not accepting new accounts right now' },
      },
    ])
    renderApp(<SignupPage />)

    await userEvent.type(screen.getByLabelText(/name/i), 'Someone')
    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /create account/i }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'Drive is not accepting new accounts right now',
    )
  })
})

describe('an unverified account signing in', () => {
  // Deliberately not the wording the server ships. A screen that recognised
  // this refusal by matching its copy would keep passing against the real
  // message and silently stop offering the button the moment it was reworded.
  const refusal = {
    code: 'email_unverified',
    message: 'the mailbox on this account has not been confirmed yet',
  }

  it('offers a fresh verification link, and sends one for the address that was typed', async () => {
    const calls = stubFetch([
      { method: 'POST', path: '/api/auth/login', status: 401, body: refusal },
      { method: 'POST', path: '/api/auth/resend-verification', body: { status: 'ok' } },
    ])
    renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    await userEvent.click(await screen.findByRole('button', { name: 'Resend verification' }))

    await waitFor(() => {
      const post = calls.find((c) => c.url === '/api/auth/resend-verification')
      expect(post?.method).toBe('POST')
      expect(post?.body).toEqual({ email: 'someone@example.test' })
      expect(post?.headers['X-Drive-Client']).toBe('web')
    })
    // Once, and then it says so — a button that stays offering to send again
    // invites spending the account's whole mail budget from one screen.
    expect(await screen.findByText(/a fresh link is on its way/i)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Resend verification' })).toBeNull()
  })

  it('offers nothing to resend when the credentials were simply wrong', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/login',
        status: 401,
        body: { code: 'unauthorized', message: 'that email and password combination is not right' },
      },
    ])
    renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    await screen.findByRole('alert')
    expect(screen.queryByRole('button', { name: 'Resend verification' })).toBeNull()
  })

  it('points at the reset screen for the other way to be locked out', () => {
    stubFetch([])
    renderApp(<LoginPage />)
    expect(screen.getByRole('link', { name: 'Forgot password?' }).getAttribute('href')).toBe('/forgot')
  })
})

describe('/forgot', () => {
  it('asks for a reset link and promises nothing about the address', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/password-reset', body: { status: 'ok' } }])
    renderApp(<ForgotPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))

    expect(await screen.findByText(/has an account, a reset link is on its way/i)).toBeTruthy()
    expect(calls[0].body).toEqual({ email: 'someone@example.test' })
    expect(calls[0].headers['X-Drive-Client']).toBe('web')
  })

  it('says the same thing however much the answer gives away', async () => {
    // The endpoint answers 200 for an address with no account. If the screen
    // ever branched on something in that body — an `exists` flag, a status
    // other than "ok" — the deliberately silent endpoint would become an
    // account-existence oracle again, on the client side this time.
    stubFetch([
      { method: 'POST', path: '/api/auth/password-reset', body: { status: 'ok', exists: false, sent: false } },
    ])
    renderApp(<ForgotPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'nobody@example.test')
    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))

    expect(await screen.findByText(/has an account, a reset link is on its way/i)).toBeTruthy()
    expect(screen.queryByText(/no account|not found|isn.t registered|doesn.t exist/i)).toBeNull()
  })

  it('stays calm when one address has asked too often', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/password-reset',
        status: 429,
        body: { code: 'rate_limited', message: 'too many requests' },
      },
    ])
    renderApp(<ForgotPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'That\u2019s a few reset links in a short time. Try again in a little while.',
    )
  })
})

describe('/reset', () => {
  it('redeems the token from the query string with the new password', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/password-reset/confirm', status: 204 }])
    // Padded on purpose: a link copied out of a mail client picks up whitespace
    // often enough, and the server would spend this token's one redemption on
    // rejecting it.
    renderApp(<ResetPage />, { route: '/reset?token=%20reset-tok-1%20' })

    await userEvent.type(screen.getByLabelText('New password'), 'a-new-passphrase')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'a-new-passphrase')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(await screen.findByText(/your new password is in place/i)).toBeTruthy()
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/auth/password-reset/confirm')
    expect(calls[0].body).toEqual({ token: 'reset-tok-1', new_password: 'a-new-passphrase' })
    expect(calls[0].headers['X-Drive-Client']).toBe('web')
  })

  it('shows the server\u2019s own words on a spent link, and a way to get another', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/password-reset/confirm',
        status: 422,
        body: { code: 'invalid', message: 'this reset link is invalid or has expired' },
      },
    ])
    renderApp(<ResetPage />, { route: '/reset?token=stale' })

    await userEvent.type(screen.getByLabelText('New password'), 'a-new-passphrase')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'a-new-passphrase')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'this reset link is invalid or has expired',
    )
    // A dead end otherwise: that link will never work again, so the way out has
    // to be on the screen that reports it.
    expect((await screen.findByRole('link', { name: 'Send another reset link' })).getAttribute('href')).toBe('/forgot')
  })

  it('clears the mismatch from whichever of the two fields is corrected', async () => {
    const calls = stubFetch([])
    renderApp(<ResetPage />, { route: '/reset?token=reset-tok-1' })

    await userEvent.type(screen.getByLabelText('New password'), 'a-new-passphrase')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'a-different-passphrase')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Those two passwords don’t match.')
    expect(calls).toHaveLength(0)

    // Retyping the top field is as much a correction as retyping the bottom
    // one, and an alert that outlives the disagreement it describes is telling
    // the person their form is wrong while they look at a form that is right.
    await userEvent.clear(screen.getByLabelText('New password'))
    await userEvent.type(screen.getByLabelText('New password'), 'a-different-passphrase')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('never posts an empty token', async () => {
    const calls = stubFetch([])
    renderApp(<ResetPage />, { route: '/reset' })

    expect(await screen.findByText(/missing its token/i)).toBeTruthy()
    expect(screen.queryByLabelText('New password')).toBeNull()
    expect(calls).toHaveLength(0)
  })
})
