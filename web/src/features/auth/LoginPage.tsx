import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { login } from '../../lib/api'
import { AuthCard, buttonClass, FormError, inputClass } from '../../ui/controls'
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

  return (
    <AuthCard title="Sign in to Drive">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          mutation.mutate()
        }}
      >
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
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <FormError error={mutation.error} />
        <button className={buttonClass} type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
      <p className="text-sm text-neutral-600">
        No account?{' '}
        <Link className="underline" to="/signup">
          Sign up
        </Link>
      </p>
    </AuthCard>
  )
}
