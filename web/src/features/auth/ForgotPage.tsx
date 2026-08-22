import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { ApiError, requestReset } from '../../lib/api'
import { AuthCard, buttonClass, fieldClass, FormError, inputClass } from '../../ui/controls'
import { emailHint, invalidEmailClass, isPlausibleEmail } from './email'

/**
 * `/forgot` — ask for a reset link.
 *
 * The server answers 200 for every syntactically valid address, whether or not
 * an account exists, and this screen has to hold the same line: one success
 * state, phrased conditionally, reached by every address. A page that said
 * "sent" for a known address and "no such account" for an unknown one would
 * turn a deliberately silent endpoint back into an account-existence oracle,
 * which is exactly what the login form refuses to be.
 */
export function ForgotPage() {
  const [email, setEmail] = useState('')
  const [emailJudged, setEmailJudged] = useState(false)

  const emailBad = emailJudged && !isPlausibleEmail(email)

  const mutation = useMutation({ mutationFn: () => requestReset(email) })

  if (mutation.isSuccess) {
    return (
      <AuthCard title="Check your inbox">
        <p className="text-sm text-ink-2">
          If <span className="font-medium text-ink">{email}</span> has an account, a reset link is on its way. It
          expires in an hour.
        </p>
        <Link className="text-[13px] font-medium text-teal hover:underline" to="/login">
          Back to sign in
        </Link>
      </AuthCard>
    )
  }

  const throttled = mutation.error instanceof ApiError && mutation.error.code === 'rate_limited'

  return (
    <AuthCard title="Reset your password">
      <p className="text-sm text-ink-2">
        Give the address the account uses and we’ll send a link to set a new password.
      </p>
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
          mutation.mutate()
        }}
      >
        <div className="flex flex-col gap-1.5">
          <label className={fieldClass}>
            Email
            <input
              className={`${inputClass} ${invalidEmailClass}`}
              type="email"
              name="email"
              autoComplete="email"
              inputMode="email"
              spellCheck={false}
              required
              aria-invalid={emailBad || undefined}
              aria-describedby={emailBad ? 'forgot-email-hint' : undefined}
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              onBlur={() => {
                // An untouched empty field is not a mistake yet; leaving one
                // half-typed is.
                if (email.trim() !== '') setEmailJudged(true)
              }}
            />
          </label>
          {emailBad && (
            <p id="forgot-email-hint" className="text-[13px] text-danger">
              {emailHint(email)}
            </p>
          )}
        </div>
        {throttled ? (
          <p role="alert" className="rounded-control bg-warn-soft px-3 py-2 text-[13px] text-warn">
            That’s a few reset links in a short time. Try again in a little while.
          </p>
        ) : (
          <FormError error={mutation.error} />
        )}
        <button className={buttonClass} type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Sending…' : 'Send reset link'}
        </button>
      </form>
      <p className="text-[13px] text-ink-3">
        Remembered it?{' '}
        <Link className="font-medium text-teal hover:underline" to="/login">
          Sign in
        </Link>
      </p>
    </AuthCard>
  )
}
