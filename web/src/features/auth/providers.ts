/**
 * Which other ways in this deployment offers.
 *
 * Asked once per browser session and kept (`staleTime: Infinity`): the answer
 * is a deployment's configuration, not a fact about the person, so it cannot
 * change while the tab is open. Any failure — offline, a 500, a clone whose
 * server predates the route — reads as "not configured", because the only
 * thing this answer decides is whether to draw one extra button, and drawing
 * one that cannot work is worse than not drawing it.
 */

import { useQuery } from '@tanstack/react-query'

import { getProviders } from '../../lib/api'

const providersKey = ['auth', 'providers'] as const

/**
 * `settled` is whether the answer has actually arrived — a failure counts, since
 * that is an answer of "not configured". Without it `google: false` reads the
 * same before the question has been asked as after it came back no, and a screen
 * that hides something on the strength of it flickers on every cold load.
 */
export function useProviders(): { google: boolean; settled: boolean } {
  const { data, isPending } = useQuery({
    queryKey: providersKey,
    queryFn: getProviders,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    retry: false,
  })
  return { google: data?.google === true, settled: !isPending }
}
