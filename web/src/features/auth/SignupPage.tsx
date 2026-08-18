import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { signup } from '../../lib/api'
import { AuthCard, buttonClass, FormError, inputClass } from '../../ui/controls'

export function SignupPage() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [displayName, setDisplayName] = useState('')

  const mutation = useMutation({ mutationFn: () => signup(email, password, displayName) })

  if (mutation.isSuccess) {
    return (
      <AuthCard title="Check your inbox">
        {/* Signup answers identically whether or not the address was free —
            it must not report whether an account exists — so this copy claims
            no more than the server actually promises. */}
        <p className="text-sm text-neutral-600">
          If <span className="font-medium">{email}</span> is new here, a verification link is on its way. Open it to
          finish setting up the account.
        </p>
        <Link className="text-sm underline" to="/login">
          Back to sign in
        </Link>
      </AuthCard>
    )
  }

  return (
    <AuthCard title="Create a Drive account">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          mutation.mutate()
        }}
      >
        <label className="flex flex-col gap-1 text-sm">
          Name
          <input
            className={inputClass}
            name="display_name"
            autoComplete="name"
            required
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
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
        <label className="flex flex-col gap-1 text-sm">
          Password
          <input
            className={inputClass}
            type="password"
            name="password"
            autoComplete="new-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <FormError error={mutation.error} />
        <button className={buttonClass} type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Creating…' : 'Create account'}
        </button>
      </form>
      <p className="text-sm text-neutral-600">
        Already have an account?{' '}
        <Link className="underline" to="/login">
          Sign in
        </Link>
      </p>
    </AuthCard>
  )
}
