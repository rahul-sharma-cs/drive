import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { signup } from '../../lib/api'
import { AuthCard, buttonClass, fieldClass, FormError, inputClass } from '../../ui/controls'
import { emailHint, isPlausibleEmail } from './email'
import { invalidFieldClass } from './fields'
import { isAcceptablePassword, passwordHint } from './password'

export function SignupPage() {
  const [email, setEmail] = useState('')
  const [emailJudged, setEmailJudged] = useState(false)
  const [password, setPassword] = useState('')
  const [passwordJudged, setPasswordJudged] = useState(false)
  const [displayName, setDisplayName] = useState('')

  // Judged on the way out of the field, not on every keystroke: nobody wants to
  // be told their address is wrong while they are still halfway through it.
  const emailBad = emailJudged && !isPlausibleEmail(email)
  const passwordBad = passwordJudged && !isAcceptablePassword(password)

  const mutation = useMutation({ mutationFn: () => signup(email, password, displayName) })

  if (mutation.isSuccess) {
    return (
      <AuthCard title="Check your inbox">
        {/* Signup answers identically whether or not the address was free —
            it must not report whether an account exists — so this copy claims
            no more than the server actually promises. */}
        <p className="text-sm text-ink-2">
          If <span className="font-medium text-ink">{email}</span> is new here, a verification link is on its way. Open it to
          finish setting up the account.
        </p>
        <Link className="text-[13px] font-medium text-teal hover:underline" to="/login">
          Back to sign in
        </Link>
      </AuthCard>
    )
  }

  return (
    <AuthCard title="Create a Drive account">
      <form
        className="flex flex-col gap-3 rounded-card border border-line bg-surface p-5 shadow-card"
        // The browser's own bubble is off: it appears over the field, says its
        // own thing, and vanishes on the next click. The hint under the field
        // stays put until the address is fixed.
        noValidate
        onSubmit={(e) => {
          e.preventDefault()
          // Pressing the button counts as judging the fields — otherwise a
          // submit with a bad address in it would just quietly do nothing.
          setEmailJudged(true)
          setPasswordJudged(true)
          if (!isPlausibleEmail(email) || !isAcceptablePassword(password)) return
          mutation.mutate()
        }}
      >
        <label className={fieldClass}>
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
              aria-invalid={emailBad || undefined}
              aria-describedby={emailBad ? 'signup-email-hint' : undefined}
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
            <p id="signup-email-hint" className="text-[13px] text-danger">
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
              autoComplete="new-password"
              required
              aria-invalid={passwordBad || undefined}
              aria-describedby="signup-password-hint"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              onBlur={() => {
                if (password !== '') setPasswordJudged(true)
              }}
            />
          </label>
          {/* The rule, on screen before it is broken. It is the same line
              either way — only its colour changes — so nothing appears or
              moves under the field at the moment the person gets it wrong. */}
          <p
            id="signup-password-hint"
            className={`text-[13px] ${passwordBad ? 'text-danger' : 'text-ink-3'}`}
          >
            {passwordHint(password)}
          </p>
        </div>
        <FormError error={mutation.error} />
        <button className={buttonClass} type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Creating…' : 'Create account'}
        </button>
      </form>
      <p className="text-[13px] text-ink-3">
        Already have an account?{' '}
        <Link className="font-medium text-teal hover:underline" to="/login">
          Sign in
        </Link>
      </p>
    </AuthCard>
  )
}
