/**
 * "Continue with Google", and the line the sign-in screen shows when a round
 * trip to Google came back with nothing.
 *
 * The button is a plain `<a href>`, and that is the whole design of it. It has
 * to be a real top-level navigation: react-router would swallow the click and
 * route the SPA to a path the SPA does not have, and going through
 * `lib/api.ts`'s `request()` would make it an XHR carrying `X-Drive-Client` —
 * an authorization redirect is neither of those things. Nothing in this file
 * fetches.
 *
 * Text only, in the outline variant: Google's guidelines require their own
 * asset or their approved CSS with the four-colour mark, unmodified, and an
 * approximation drawn by hand is the one thing they explicitly forbid.
 */

import { useSearchParams } from 'react-router'

import { buttonVariants } from '@/components/ui/button'

import { useProviders } from './providers'

/** The server route. Not a client route — never hand it to `<Link>`. */
export const GOOGLE_START = '/api/auth/google/start'

export function GoogleSignIn() {
  const { google } = useProviders()
  // A deployment with no Google client configured keeps exactly the screen it
  // had before this feature existed — no button, no divider, no gap.
  if (!google) return null

  return (
    <div className="flex flex-col gap-4">
      <a className={buttonVariants({ variant: 'outline', className: 'w-full' })} href={GOOGLE_START}>
        Continue with Google
      </a>
      <div className="flex items-center gap-3" aria-hidden="true">
        <span className="h-px flex-1 bg-line" />
        <span className="text-[12px] text-ink-3">or</span>
        <span className="h-px flex-1 bg-line" />
      </div>
    </div>
  )
}

/**
 * What `/login?error=…` says. The redirect is the only thing that comes back
 * from a failed round trip — there is no response body to read — so the reason
 * arrives as a query parameter, and every underlying cause but one shares a
 * single wording: the server refuses to say whether it was a bad state, an
 * unverified address or Google being down, and this screen must not guess.
 *
 * Dismissing takes the parameter out of the URL, so a reload is not shown a
 * failure that has already been read and answered.
 */
export function GoogleSignInError() {
  const [params, setParams] = useSearchParams()
  const error = params.get('error')
  if (error !== 'google' && error !== 'google_closed') return null

  const message =
    error === 'google_closed'
      ? 'This Drive is not accepting new accounts.'
      : "Google sign-in didn't complete. Try again, or use your email and password."

  return (
    <p role="alert" className="flex items-start gap-3 text-[13px] text-ink-2">
      <span className="flex-1">{message}</span>
      <button
        type="button"
        className="shrink-0 font-medium text-teal hover:underline"
        onClick={() => {
          const next = new URLSearchParams(params)
          next.delete('error')
          // Replace: the failed attempt is not a place to go back to.
          setParams(next, { replace: true })
        }}
      >
        Dismiss
      </button>
    </p>
  )
}
