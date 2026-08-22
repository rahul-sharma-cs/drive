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
import { useSetSession } from './session'

export function LoginPage() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const navigate = useNavigate()
  const setSession = useSetSession()

  // Login answers with the whole `me` shape, so the session cache is filled
  // from the response rather than by a second /auth/me round trip — which the
  // per-IP auth bucket would also count.
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

  return (
    <AuthCard title="Sign in to Drive">
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
        <label className={fieldClass}>
          Password
          <input
            className={inputClass}
            type="password"
            name="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <FormError error={mutation.error} />
        {unverified &&
          (resend.isSuccess ? (
            <p className="text-[13px] text-ink-2">
              If that account still needs verifying, a fresh link is on its way.
            </p>
          ) : (
            <button
              type="button"
              className={secondaryButtonClass}
              disabled={resend.isPending}
              onClick={() => resend.mutate()}
            >
              {resend.isPending ? 'Sending…' : 'Resend verification'}
            </button>
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
