/**
 * Test harness for component tests: one fresh QueryClient per test (a shared
 * one leaks a cached `me` between tests), a router, since every screen uses
 * links or navigation, and the tooltip provider.
 *
 * The provider is here rather than in each test because the app mounts it once
 * in `AppLayout`, above every screen — a component test that renders a screen
 * directly is standing in for that layout, and a `Tooltip` without a provider
 * throws. Leaving it out is what kept the command band's icon buttons
 * tooltip-less: they could not be given one that the tests could render.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactNode } from 'react'
import { MemoryRouter } from 'react-router'
import { vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'

export function renderApp(
  ui: ReactNode,
  { route = '/', seed }: { route?: string; seed?: (client: QueryClient) => void } = {},
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  })
  seed?.(client)
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <TooltipProvider delayDuration={400}>{ui}</TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...result, client }
}

export interface StubbedCall {
  method: string
  url: string
  /**
   * What the request actually carried, not the object the caller happened to
   * pass. A plain record only reads back headers written as a plain object: a
   * call that built a `Headers` instead would read as carrying none at all,
   * and every assertion that a header is *absent* would pass without being
   * true — which is exactly the assertion the preview's bare cross-origin GET
   * rests on.
   */
  headers: Headers
  body: unknown
}

export interface StubRoute {
  method?: string
  /** Matched against the request URL: exact string, or a regex. */
  path: string | RegExp
  status?: number
  body?: unknown
  /**
   * Held until this settles, for a case about what a screen draws while a
   * request is still outstanding. Without it every stubbed answer lands on the
   * next microtask, and the in-flight state no screen is ever seen in from a
   * test is exactly where a hide-on-`false` guard flickers.
   */
  hold?: Promise<unknown>
}

/**
 * Replaces global fetch with canned answers matched by method+URL, and records
 * every call. Matching beats a response queue because screens fire concurrent
 * requests (children and breadcrumbs) whose order is not defined, and because a
 * refetch after a mutation must be able to hit the same route twice.
 *
 * Screens talk to the real `lib/api` client, so the recorded calls cover the
 * wire shape — the header the CSRF gate requires included.
 */
/**
 * Answers every screen needs and almost no test is about: whether the
 * deployment offers a third-party sign-in, and what is linked to the account.
 * Both are asked by chrome that sits on screens whose tests are about something
 * else entirely, so they are answered here rather than in a dozen route lists.
 *
 * They come last, so a case that says otherwise wins the match, and they are
 * the only defaults: an unstubbed request still throws, which is what keeps a
 * missing stub from passing as an empty answer.
 */
const DEFAULTS: StubRoute[] = [
  { path: '/api/auth/providers', body: { google: false } },
  { path: '/api/auth/identities', body: { items: [], next_cursor: null } },
]

export function stubFetch(routes: StubRoute[]) {
  const calls: StubbedCall[] = []
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const method = init.method ?? 'GET'
    const matches = (r: StubRoute) =>
      (r.method ?? 'GET') === method && (typeof r.path === 'string' ? r.path === url : r.path.test(url))
    const asked = routes.find(matches)
    const fallback = asked ? undefined : DEFAULTS.find(matches)
    // A call the harness answered on its own is not traffic the screen under
    // test made a choice about, and recording it would make every "and sends
    // nothing" assertion count it. A case that wants to assert on one of those
    // routes stubs it itself, which puts it back in the record.
    if (!fallback) {
      calls.push({
        method,
        url,
        headers: new Headers(init.headers as HeadersInit | undefined),
        body: init.body === undefined ? undefined : JSON.parse(init.body as string),
      })
    }
    const route = asked ?? fallback
    if (!route) throw new Error(`unstubbed request: ${method} ${url}`)
    if (route.hold) await route.hold
    // A 204 may carry no body at all — the Response constructor throws on one.
    const payload = route.body === undefined ? null : JSON.stringify(route.body)
    return new Response(payload, {
      status: route.status ?? 200,
      headers: { 'Content-Type': 'application/json' },
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return calls
}
