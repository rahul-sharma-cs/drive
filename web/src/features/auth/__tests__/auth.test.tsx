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
    expect(calls[0].headers.get('X-Drive-Client')).toBe('web')
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
      expect(post?.headers.get('X-Drive-Client')).toBe('web')
    })
    // Once, and then it says so — a button that stays offering to send again
    // invites spending the account's whole mail budget from one screen.
    expect(await screen.findByText(/a fresh link is on its way/i)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Resend verification' })).toBeNull()
  })

  it('shows the mailer’s refusal instead of failing back to an unchanged button', async () => {
    stubFetch([
      { method: 'POST', path: '/api/auth/login', status: 401, body: refusal },
      {
        method: 'POST',
        path: '/api/auth/resend-verification',
        status: 429,
        body: { code: 'rate_limited', message: 'too many requests. Try again later.' },
      },
    ])
    renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await userEvent.click(await screen.findByRole('button', { name: 'Resend verification' }))

    // A button that fails back to exactly how it looked before the press says
    // nothing happened, so the person presses it again — against a budget that
    // has already refused them once.
    await waitFor(() =>
      expect(screen.getAllByRole('alert').map((a) => a.textContent)).toContain('too many requests. Try again later.'),
    )

    await userEvent.clear(screen.getByLabelText(/email/i))
    await userEvent.type(screen.getByLabelText(/email/i), 'someone-else@example.test')
    // That refusal was about the mailbox that was in the field at the time.
    expect(screen.getAllByRole('alert').map((a) => a.textContent)).not.toContain('too many requests. Try again later.')
  })

  it('stops claiming a link is on its way once the address underneath it changes', async () => {
    stubFetch([
      { method: 'POST', path: '/api/auth/login', status: 401, body: refusal },
      { method: 'POST', path: '/api/auth/resend-verification', body: { status: 'ok' } },
    ])
    renderApp(<LoginPage />)

    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await userEvent.click(await screen.findByRole('button', { name: 'Resend verification' }))
    expect(await screen.findByText(/a fresh link is on its way/i)).toBeTruthy()

    await userEvent.clear(screen.getByLabelText(/email/i))
    await userEvent.type(screen.getByLabelText(/email/i), 'someone-else@example.test')

    // Someone correcting a typo in the address is the likeliest person to be
    // here. Left standing, that sentence tells them a link is on its way to the
    // address they have just typed — and hides the button that would send one.
    expect(screen.queryByText(/a fresh link is on its way/i)).toBeNull()
    expect(screen.getByRole('button', { name: 'Resend verification' })).toBeTruthy()
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
    expect(calls[0].headers.get('X-Drive-Client')).toBe('web')
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
    expect(calls[0].headers.get('X-Drive-Client')).toBe('web')
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

  it('offers another link only when this one is spent, never when the server was simply busy', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/password-reset/confirm',
        status: 429,
        body: { code: 'rate_limited', message: 'we are busy right now. Try again in a moment.' },
      },
    ])
    renderApp(<ResetPage />, { route: '/reset?token=reset-tok-1' })

    await userEvent.type(screen.getByLabelText('New password'), 'a-new-passphrase')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'a-new-passphrase')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'we are busy right now. Try again in a moment.',
    )
    // The link in hand is still good — the server just could not do the work
    // this second. Sending the person off to ask for a new one spends the one
    // they have on a failure that had nothing to do with it.
    expect(screen.queryByRole('link', { name: 'Send another reset link' })).toBeNull()
  })

  it('never posts an empty token', async () => {
    const calls = stubFetch([])
    renderApp(<ResetPage />, { route: '/reset' })

    expect(await screen.findByText(/missing its token/i)).toBeTruthy()
    expect(screen.queryByLabelText('New password')).toBeNull()
    expect(calls).toHaveLength(0)
  })
})

/**
 * The screens' half of the shape check. `email.test.ts` owns the rule itself;
 * what matters here is that a field which fails it says so on the way out —
 * and that the form does not hand the address to the API anyway.
 */
describe('an address that cannot be an email', () => {
  const bad = 'wfwef@fweffwef'
  const fixed = 'wfwef@fweffwef.example'

  it('marks the field on /signup, holds the request back, and clears once it is fixed', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/signup', body: { status: 'ok' } }])
    renderApp(<SignupPage />)

    const field = screen.getByLabelText(/email/i)
    await userEvent.type(screen.getByLabelText(/name/i), 'Someone')
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.type(field, bad)
    await userEvent.tab()

    expect(field.getAttribute('aria-invalid')).toBe('true')
    const hint = screen.getByText(/look like an email address/i)
    expect(field.getAttribute('aria-describedby')).toBe(hint.getAttribute('id'))

    await userEvent.click(screen.getByRole('button', { name: /create account/i }))
    // The button stays pressable — a disabled one is the same silence the
    // native bubble left behind.
    expect(screen.getByRole('button', { name: /create account/i })).toHaveProperty('disabled', false)
    expect(calls).toHaveLength(0)

    await userEvent.type(field, '.example')
    expect(screen.queryByText(/look like an email address/i)).toBeNull()
    expect(field.getAttribute('aria-invalid')).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: /create account/i }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body).toEqual({ email: fixed, password: 'hunter2hunter2', display_name: 'Someone' })
  })

  it('marks the field on /login, holds the request back, and clears once it is fixed', async () => {
    const user = {
      id: 'u1',
      email: fixed,
      display_name: 'Someone',
      root_id: 'r1',
      email_verified_at: '2026-08-17T00:00:00Z',
    }
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/login', body: user }])
    renderApp(<LoginPage />)

    const field = screen.getByLabelText(/email/i)
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2hunter2')
    await userEvent.type(field, bad)
    await userEvent.tab()

    expect(field.getAttribute('aria-invalid')).toBe('true')
    expect(screen.getByText(/look like an email address/i)).toBeTruthy()

    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    expect(calls).toHaveLength(0)

    await userEvent.type(field, '.example')
    expect(screen.queryByText(/look like an email address/i)).toBeNull()
    expect(field.getAttribute('aria-invalid')).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body).toEqual({ email: fixed, password: 'hunter2hunter2' })
  })

  it('marks the field on /forgot, holds the request back, and clears once it is fixed', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/password-reset', body: { status: 'ok' } }])
    renderApp(<ForgotPage />)

    const field = screen.getByLabelText(/email/i)
    await userEvent.type(field, bad)
    await userEvent.tab()

    expect(field.getAttribute('aria-invalid')).toBe('true')
    expect(screen.getByText(/look like an email address/i)).toBeTruthy()

    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))
    expect(calls).toHaveLength(0)
    // Nothing was sent, so the screen must not have moved on to the state that
    // says a link is on its way.
    expect(screen.queryByText(/reset link is on its way/i)).toBeNull()

    await userEvent.type(field, '.example')
    expect(screen.queryByText(/look like an email address/i)).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body).toEqual({ email: fixed })
  })

  it('says something when the field is simply empty, since the native bubble is off', async () => {
    // `noValidate` turns off "please fill out this field". Without a line of
    // our own, pressing the button on an empty form would do nothing visible
    // at all — which is the complaint this whole change is about.
    const calls = stubFetch([])
    renderApp(<ForgotPage />)

    const form = document.querySelector('form')
    expect(form?.hasAttribute('novalidate')).toBe(true)

    await userEvent.click(screen.getByRole('button', { name: 'Send reset link' }))

    expect(screen.getByText('Enter an email address')).toBeTruthy()
    expect(screen.getByLabelText(/email/i).getAttribute('aria-invalid')).toBe('true')
    expect(calls).toHaveLength(0)
  })

  it('leaves a field alone until it has been typed in', async () => {
    // Tabbing through a form should not paint every untouched field red.
    stubFetch([])
    renderApp(<ForgotPage />)

    const field = screen.getByLabelText(/email/i)
    await userEvent.click(field)
    await userEvent.tab()

    expect(field.getAttribute('aria-invalid')).toBeNull()
    expect(screen.queryByText(/look like an email address/i)).toBeNull()
  })
})

/**
 * The password half of the same idea: say the rule before it is broken, and
 * when a sign-in is refused, colour the two fields the refusal is about.
 */
describe('a password the server would refuse', () => {
  it('shows the rule on /signup, turns it red when it is broken, and sends nothing', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/signup', body: { status: 'ok' } }])
    renderApp(<SignupPage />)

    const field = screen.getByLabelText(/^password$/i)
    // The rule is on screen from the start, quiet — not sprung on somebody
    // after they have already got it wrong.
    const hint = screen.getByText('At least 8 characters')
    expect(hint.className).toContain('text-ink-3')
    expect(field.getAttribute('aria-describedby')).toBe(hint.getAttribute('id'))
    expect(field.getAttribute('aria-invalid')).toBeNull()

    await userEvent.type(screen.getByLabelText(/name/i), 'Someone')
    await userEvent.type(screen.getByLabelText(/email/i), 'someone@example.test')
    await userEvent.type(field, 'short')
    await userEvent.tab()

    expect(field.getAttribute('aria-invalid')).toBe('true')
    expect(screen.getByText('At least 8 characters').className).toContain('text-danger')

    await userEvent.click(screen.getByRole('button', { name: /create account/i }))
    expect(calls).toHaveLength(0)

    // Nine characters now, and the field stops being wrong the moment it is.
    await userEvent.type(field, 'ened')
    expect(field.getAttribute('aria-invalid')).toBeNull()
    expect(screen.getByText('At least 8 characters').className).toContain('text-ink-3')

    await userEvent.click(screen.getByRole('button', { name: /create account/i }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body).toEqual({
      email: 'someone@example.test',
      password: 'shortened',
      display_name: 'Someone',
    })
  })
})

describe('a refused sign-in', () => {
  const refusal = {
    code: 'unauthorized',
    message: 'that email and password combination is not right',
  }

  it('colours both fields and puts the server’s own words under the password', async () => {
    stubFetch([{ method: 'POST', path: '/api/auth/login', status: 401, body: refusal }])
    renderApp(<LoginPage />)

    const email = screen.getByLabelText(/email/i)
    const password = screen.getByLabelText(/password/i)
    await userEvent.type(email, 'someone@example.test')
    await userEvent.type(password, 'not-the-password')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    // Both, because the server did not say which one was wrong -- and it never
    // will: "no such account" would be an account-existence oracle.
    await waitFor(() => expect(email.getAttribute('aria-invalid')).toBe('true'))
    expect(password.getAttribute('aria-invalid')).toBe('true')

    const message = screen.getByText(refusal.message)
    expect(password.getAttribute('aria-describedby')).toBe(message.getAttribute('id'))
    expect(screen.queryByText(/no such account|not registered|unknown email/i)).toBeNull()
    // Once, not twice: the form-level error must not restate what the field
    // already says.
    expect(screen.getAllByText(refusal.message)).toHaveLength(1)
  })

  it('lets go of the red as soon as either field is edited', async () => {
    stubFetch([{ method: 'POST', path: '/api/auth/login', status: 401, body: refusal }])
    renderApp(<LoginPage />)

    const email = screen.getByLabelText(/email/i)
    const password = screen.getByLabelText(/password/i)
    await userEvent.type(email, 'someone@example.test')
    await userEvent.type(password, 'not-the-password')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await waitFor(() => expect(password.getAttribute('aria-invalid')).toBe('true'))

    // Editing the password: that refusal was about a pair that no longer
    // exists, so it stops being shown about this one.
    await userEvent.type(password, '!')
    expect(password.getAttribute('aria-invalid')).toBeNull()
    expect(email.getAttribute('aria-invalid')).toBeNull()
    expect(screen.queryByText(refusal.message)).toBeNull()
  })

  it('lets go of the red when the email is the half being fixed', async () => {
    stubFetch([{ method: 'POST', path: '/api/auth/login', status: 401, body: refusal }])
    renderApp(<LoginPage />)

    const email = screen.getByLabelText(/email/i)
    const password = screen.getByLabelText(/password/i)
    await userEvent.type(email, 'someone@example.test')
    await userEvent.type(password, 'not-the-password')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await waitFor(() => expect(email.getAttribute('aria-invalid')).toBe('true'))

    await userEvent.type(email, 'x')
    expect(email.getAttribute('aria-invalid')).toBeNull()
    expect(password.getAttribute('aria-invalid')).toBeNull()
  })

  it('stays calm for a spent budget, which says nothing about the credentials', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/login',
        status: 429,
        body: {
          code: 'rate_limited',
          message: 'too many sign-in attempts for this address. Try again in a few minutes.',
        },
      },
    ])
    renderApp(<LoginPage />)

    const email = screen.getByLabelText(/email/i)
    const password = screen.getByLabelText(/password/i)
    await userEvent.type(email, 'someone@example.test')
    await userEvent.type(password, 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    expect(await screen.findByText(/too many sign-in attempts/i)).toBeTruthy()
    // The credentials were never judged, so nothing about them is wrong yet.
    expect(email.getAttribute('aria-invalid')).toBeNull()
    expect(password.getAttribute('aria-invalid')).toBeNull()
  })

  it('does not colour the fields for an unverified address, whose password was right', async () => {
    stubFetch([
      {
        method: 'POST',
        path: '/api/auth/login',
        status: 401,
        body: { code: 'email_unverified', message: 'verify your email first: check your inbox' },
      },
    ])
    renderApp(<LoginPage />)

    const email = screen.getByLabelText(/email/i)
    const password = screen.getByLabelText(/password/i)
    await userEvent.type(email, 'someone@example.test')
    await userEvent.type(password, 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }))

    // This one is also a 401, and it means the opposite: the credentials were
    // accepted. Painting them red would be telling somebody to fix what they
    // got right.
    expect(await screen.findByRole('button', { name: 'Resend verification' })).toBeTruthy()
    expect(email.getAttribute('aria-invalid')).toBeNull()
    expect(password.getAttribute('aria-invalid')).toBeNull()
  })
})

describe('/reset', () => {
  it('holds a too-short new password back before it costs the link a redemption', async () => {
    const calls = stubFetch([{ method: 'POST', path: '/api/auth/password-reset/confirm', status: 204 }])
    renderApp(<ResetPage />, { route: '/reset?token=tok-1' })

    const field = screen.getByLabelText('New password')
    expect(screen.getByText('At least 8 characters').className).toContain('text-ink-3')

    await userEvent.type(field, 'short')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'short')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(field.getAttribute('aria-invalid')).toBe('true')
    expect(screen.getByText('At least 8 characters').className).toContain('text-danger')
    // A reset token is spent by the attempt, not by the outcome.
    expect(calls).toHaveLength(0)
  })
})
