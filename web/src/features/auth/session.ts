/**
 * Who is signed in.
 *
 * Fetched once and kept: `staleTime: Infinity` plus no refetch-on-focus. Who
 * you are changes only when this tab changes it — signing in, signing out,
 * renaming the account — and every one of those writes the cache from the
 * answer it already has, so a refetch on every window focus would spend a
 * request per tab switch to be told the same thing.
 *
 * `GET /auth/me` is behind RequireAuth, and the server's per-IP `/api/auth/*`
 * bucket is in front of the unauthenticated half of that surface — plus
 * `POST /auth/password`, the one authenticated route that keeps it. So this
 * query is not up against that limiter; it is simply not worth re-asking.
 */

import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'

import { ApiError, me, type Me } from '../../lib/api'
import { shareUrls } from '../share/shareUrls'

export const meKey = ['me'] as const

export function useMe() {
  return useQuery({
    queryKey: meKey,
    queryFn: async (): Promise<Me | null> => {
      try {
        return await me()
      } catch (e) {
        // Anonymous is an answer, not a failure: 401 is what the server says
        // to a browser with no cookie, and it must not surface as an error.
        if (e instanceof ApiError && e.status === 401) {
          // No session in this browser means no share links kept in it. This
          // is the one place a sign-out that happened elsewhere — the session
          // lapsing, "Sign out everywhere" from another device — reaches this
          // browser, and the in-browser sign-outs already clear on their own.
          shareUrls.clear()
          return null
        }
        throw e
      }
    },
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    retry: false,
  })
}

/** The signed-in user. Only valid under `RequireAuth`, which guarantees one. */
export function useSession(): Me {
  const { data } = useMe()
  if (!data) throw new Error('useSession() used outside RequireAuth')
  return data
}

export function setSession(client: QueryClient, user: Me | null): void {
  client.setQueryData(meKey, user)
}

export function useSetSession(): (user: Me | null) => void {
  const client = useQueryClient()
  return (user) => setSession(client, user)
}
