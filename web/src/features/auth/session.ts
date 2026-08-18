/**
 * Who is signed in.
 *
 * `GET /auth/me` sits inside the server's per-IP `/api/auth/*` bucket (10/min),
 * so this query is fetched once and kept: `staleTime: Infinity` plus no
 * refetch-on-focus. Login and logout write the cache directly instead of
 * triggering a refetch, which is what keeps a tab-switching user — or several
 * users behind one NAT — off the limiter.
 */

import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'

import { ApiError, me, type Me } from '../../lib/api'

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
        if (e instanceof ApiError && e.status === 401) return null
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
