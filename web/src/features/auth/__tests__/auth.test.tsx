// @vitest-environment jsdom

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { renderApp, stubFetch } from '../../../test/render'
import { LoginPage } from '../LoginPage'
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
    // /auth/me would only spend the per-IP auth budget.
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
