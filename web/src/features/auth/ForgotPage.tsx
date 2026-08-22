import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { ApiError, requestReset } from '../../lib/api'
import { AuthCard, buttonClass, fieldClass, FormError, inputClass } from '../../ui/controls'

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
        onSubmit={(e) => {
          e.preventDefault()
          mutation.mutate()
        }}
      >
        <label className={fieldClass}>
          Email
          <input
            className={inputClass}
            type="email"
            name="email"
            autoComplete="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
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
