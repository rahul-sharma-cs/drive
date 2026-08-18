/**
 * `/verify` — the landing page for the link in the verification mail
 * (`${DRIVE_BASE_URL}/verify?token=…`, PLAN §Mail construction). Without this
 * route every signup is a dead end: the account exists, the mail arrives, and
 * the link lands on a page that does nothing.
 *
 * The token is redeemed once, from a query rather than a mutation-in-an-effect:
 * StrictMode mounts effects twice in development, and a second POST would spend
 * an already-redeemed token and report failure for a verification that in fact
 * succeeded. React Query dedupes and caches by key, so the second mount reuses
 * the first result.
 */

import { useQuery } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router'

import { verifyEmail } from '../../lib/api'
import { AuthCard, buttonClass, FormError } from '../../ui/controls'

export function VerifyPage() {
  const [params] = useSearchParams()
  const token = params.get('token') ?? ''

  const { isPending, isSuccess, error } = useQuery({
    queryKey: ['verify-email', token],
    queryFn: () => verifyEmail(token),
    enabled: token !== '',
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
  })

  if (token === '') {
    return (
      <AuthCard title="Verify your email">
        <p className="text-sm text-ink-2">
          This link is missing its token. Open the link from the verification email exactly as it arrived.
        </p>
        <Link className="text-[13px] font-medium text-accent hover:underline" to="/login">
          Back to sign in
        </Link>
      </AuthCard>
    )
  }

  return (
    <AuthCard title="Verify your email">
      {isPending && <p className="text-sm text-ink-2">Verifying…</p>}
      {isSuccess && (
        <>
          <p className="text-sm text-ink-2">Your email is verified. You can sign in now.</p>
          <Link className={buttonClass} to="/login">
            Go to sign in
          </Link>
        </>
      )}
      {error && (
        <>
          <FormError error={error} />
          <p className="text-sm text-ink-2">
            Verification links expire. Sign up again with the same address to get a fresh one.
          </p>
          <Link className="text-[13px] font-medium text-accent hover:underline" to="/signup">
            Back to sign up
          </Link>
        </>
      )}
    </AuthCard>
  )
}
