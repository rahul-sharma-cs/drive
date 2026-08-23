import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useSearchParams } from 'react-router'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { ApiError, confirmReset } from '../../lib/api'
import { AuthCard, fieldClass, FormError } from '../../ui/controls'
import { shareUrls } from '../share/shareUrls'
import { isAcceptablePassword, passwordHint } from './password'

/**
 * `/reset?token=…` — the landing page for the link in the reset mail. The path
 * and the query name are fixed on the server side; without this route every
 * reset mail is a dead end.
 *
 * A missing token is answered here rather than at the server: there is nothing
 * to redeem, and posting an empty one would spend a request to be told so.
 */
export function ResetPage() {
  const client = useQueryClient()
  const [params] = useSearchParams()
  // Trimmed: a link copied out of a mail client arrives with whitespace around
  // it often enough, and the server would spend the token's one redemption on
  // rejecting it.
  const token = (params.get('token') ?? '').trim()

  const [password, setPassword] = useState('')
  const [passwordJudged, setPasswordJudged] = useState(false)
  const [confirm, setConfirm] = useState('')
  const [mismatch, setMismatch] = useState(false)

  const passwordBad = passwordJudged && !isAcceptablePassword(password)

  const mutation = useMutation({
    mutationFn: () => confirmReset(token, password),
    /**
     * This route is reachable while signed in, and the account screen sends
     * people here on purpose: an account with no password is told to set one
     * through "Forgot password". The server answers by deleting *every* session
     * for the account and clearing the cookie — so the tab that did it is now
     * signed out, while `me` is held at `staleTime: Infinity` with no refetch
     * on focus and would never be re-asked. Left alone it goes on drawing a
     * signed-in screen — the account page still saying "You sign in with
     * Google", still refusing to unlink — against a dead cookie. Everything
     * else in the cache belongs to that same session, so it goes too.
     */
    onSuccess: () => {
      client.clear()
      // The share URLs this tab minted go with it: a reset that signed this
      // tab out must not leave working links to the account's files in memory.
      shareUrls.clear()
    },
  })

  // A spent or expired link is the ordinary failure here, and the way out is
  // another link — not another attempt with the same one. Anything else is not
  // that: a busy server or a refused budget answers 429, the link in hand is
  // still good, and sending the person off to /forgot would throw it away for
  // a failure that had nothing to do with it.
  const spent =
    mutation.error instanceof ApiError && mutation.error.status === 422 && mutation.error.code === 'invalid'

  if (token === '') {
    return (
      <AuthCard title="Set a new password">
        <p className="text-sm text-ink-2">
          This reset link is missing its token. Open the link from the email exactly as it arrived, or ask for a fresh
          one.
        </p>
        <Link className="text-[13px] font-medium text-teal hover:underline" to="/forgot">
          Send another reset link
        </Link>
      </AuthCard>
    )
  }

  if (mutation.isSuccess) {
    return (
      <AuthCard title="Password set">
        <p className="text-sm text-ink-2">
          Your new password is in place, and every device that was signed in has been signed out.
        </p>
        <Button asChild>
          <Link to="/login">Go to sign in</Link>
        </Button>
      </AuthCard>
    )
  }

  return (
    <AuthCard title="Set a new password">
      <form
        className="flex flex-col gap-3 rounded-card border border-line bg-surface p-5 shadow-card"
        noValidate
        onSubmit={(e) => {
          e.preventDefault()
          // The length rule first: a password too short to be accepted is worth
          // saying so about even if the repeat below it also disagrees.
          setPasswordJudged(true)
          if (!isAcceptablePassword(password)) return
          if (password !== confirm) {
            setMismatch(true)
            mutation.reset()
            return
          }
          setMismatch(false)
          mutation.mutate()
        }}
      >
        <div className="flex flex-col gap-1.5">
          <label className={fieldClass}>
            New password
            <Input
              type="password"
              name="new_password"
              autoComplete="new-password"
              required
              aria-invalid={passwordBad || undefined}
              aria-describedby="reset-password-hint"
              value={password}
              onChange={(e) => {
                setPassword(e.target.value)
                setMismatch(false)
              }}
              onBlur={() => {
                if (password !== '') setPasswordJudged(true)
              }}
            />
          </label>
          <p
            id="reset-password-hint"
            className={`text-[13px] ${passwordBad ? 'text-danger' : 'text-ink-3'}`}
          >
            {passwordHint(password)}
          </p>
        </div>
        <label className={fieldClass}>
          Confirm new password
          <Input
            type="password"
            name="confirm_password"
            autoComplete="new-password"
            required
            value={confirm}
            onChange={(e) => {
              setConfirm(e.target.value)
              setMismatch(false)
            }}
          />
        </label>
        {mismatch ? (
          <p role="alert" className="rounded-control bg-danger-soft px-3 py-2 text-[13px] text-danger">
            Those two passwords don’t match.
          </p>
        ) : (
          <FormError error={mutation.error} />
        )}
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Setting…' : 'Set password'}
        </Button>
      </form>
      {spent && (
        <p className="text-[13px] text-ink-3">
          Reset links expire an hour after they are sent.{' '}
          <Link className="font-medium text-teal hover:underline" to="/forgot">
            Send another reset link
          </Link>
        </p>
      )}
    </AuthCard>
  )
}
