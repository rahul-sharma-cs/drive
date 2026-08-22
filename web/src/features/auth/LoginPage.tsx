import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { ApiError, login, resendVerification } from '../../lib/api'
import {
  AuthCard,
  buttonClass,
  fieldClass,
  FormError,
  inputClass,
  secondaryButtonClass,
} from '../../ui/controls'
import { emailHint, isPlausibleEmail } from './email'
import { invalidFieldClass } from './fields'
import { useSetSession } from './session'

export function LoginPage() {
  const [email, setEmail] = useState('')
  const [emailJudged, setEmailJudged] = useState(false)
  const [password, setPassword] = useState('')
  // A refusal describes the pair that was sent. Editing either half of it makes
  // the refusal a statement about credentials nobody has tried yet, so the red
  // comes off the moment the person starts fixing it.
  const [refusalDismissed, setRefusalDismissed] = useState(false)
  const navigate = useNavigate()
  const setSession = useSetSession()

  const emailBad = emailJudged && !isPlausibleEmail(email)

  // Login answers with the whole `me` shape, so the session cache is filled
  // from the response rather than by a second /auth/me round trip asking the
  // server to repeat what it has just said.
  const mutation = useMutation({
    mutationFn: () => login(email, password),
    onSuccess: (user) => {
      setSession(user)
      void navigate('/', { replace: true })
    },
  })

  const resend = useMutation({ mutationFn: () => resendVerification(email) })

  // The code, never the copy. The server owns the wording of this refusal and
  // is free to reword it; a page that recognised the state by matching the
  // message would quietly stop offering the one button that fixes it.
  const unverified = mutation.error instanceof ApiError && mutation.error.code === 'email_unverified'

  // The one refusal that is about the two fields themselves. It is deliberately
  // the same answer for a wrong password and an address with no account -- the
  // server will not say which, and neither will this screen -- so both fields
  // carry it and the message sits under the pair rather than naming one of
  // them. A spent budget (429) is not this: the credentials were never judged,
  // so nothing turns red for it.
  const refused = mutation.error instanceof ApiError && mutation.error.code === 'unauthorized'
  const showRefusal = refused && !refusalDismissed

  // Both fields stop being wrong together, because it was never established
  // which of them was.
  const clearRefusal = () => setRefusalDismissed(true)

  return (
    <AuthCard title="Sign in to Drive">
      <form
        className="flex flex-col gap-3 rounded-card border border-line bg-surface p-5 shadow-card"
        // The browser's own bubble is off: it appears over the field, says its
        // own thing, and vanishes on the next click. The hint under the field
        // stays put until the address is fixed.
        noValidate
        onSubmit={(e) => {
          e.preventDefault()
          // Pressing the button counts as judging the field — otherwise a
          // submit with a bad address in it would just quietly do nothing.
          setEmailJudged(true)
          if (!isPlausibleEmail(email)) return
          // A fresh attempt: whatever the last one was told stops being
          // dismissed, so a second wrong password paints the fields again.
          setRefusalDismissed(false)
          mutation.mutate()
        }}
      >
        <div className="flex flex-col gap-1.5">
          <label className={fieldClass}>
            Email
            <input
              className={`${inputClass} ${invalidFieldClass}`}
              type="email"
              name="email"
              autoComplete="email"
              inputMode="email"
              spellCheck={false}
              required
              aria-invalid={emailBad || showRefusal || undefined}
              aria-describedby={emailBad ? 'login-email-hint' : undefined}
              value={email}
              onChange={(e) => {
                setEmail(e.target.value)
                clearRefusal()
                // Whatever the resend last said — a link is on its way, or the
                // mailer refused — was about the address that was in this field
                // at the time. Once it is edited, both are claims about somebody
                // else's mailbox.
                resend.reset()
              }}
              onBlur={() => {
                // An untouched empty field is not a mistake yet; leaving one
                // half-typed is.
                if (email.trim() !== '') setEmailJudged(true)
              }}
            />
          </label>
          {emailBad && (
            <p id="login-email-hint" className="text-[13px] text-danger">
              {emailHint(email)}
            </p>
          )}
        </div>
        <div className="flex flex-col gap-1.5">
          <label className={fieldClass}>
            Password
            <input
              className={`${inputClass} ${invalidFieldClass}`}
              type="password"
              name="password"
              autoComplete="current-password"
              required
              aria-invalid={showRefusal || undefined}
              aria-describedby={showRefusal ? 'login-refusal' : undefined}
              value={password}
              onChange={(e) => {
                setPassword(e.target.value)
                clearRefusal()
              }}
            />
          </label>
          {showRefusal && (
            <p id="login-refusal" role="alert" className="text-[13px] text-danger">
              {(mutation.error as ApiError).message}
            </p>
          )}
        </div>
        {/* The refusal above owns its own message. Everything else -- a spent
            budget, an unverified address, a server that fell over -- still
            speaks in the form's own voice, uncoloured and unattached to a
            field. */}
        {!refused && <FormError error={mutation.error} />}
        {unverified &&
          (resend.isSuccess ? (
            <p className="text-[13px] text-ink-2">
              If that account still needs verifying, a fresh link is on its way.
            </p>
          ) : (
            <>
              {/* The mail endpoints refuse under their own budgets, and a
                  button that fails back to exactly how it looked before the
                  press invites being pressed again until one of them does. */}
              <FormError error={resend.error} />
              <button
                type="button"
                className={secondaryButtonClass}
                disabled={resend.isPending}
                onClick={() => resend.mutate()}
              >
                {resend.isPending ? 'Sending…' : 'Resend verification'}
              </button>
            </>
          ))}
        <button className={buttonClass} type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Signing in…' : 'Sign in'}
        </button>
        <Link className="self-start text-[13px] font-medium text-teal hover:underline" to="/forgot">
          Forgot password?
        </Link>
      </form>
      <p className="text-[13px] text-ink-3">
        No account?{' '}
        <Link className="font-medium text-teal hover:underline" to="/signup">
          Sign up
        </Link>
      </p>
    </AuthCard>
  )
}
