/**
 * Test harness for component tests: one fresh QueryClient per test (a shared
 * one leaks a cached `me` between tests) and a router, since every screen uses
 * links or navigation.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactNode } from 'react'
import { MemoryRouter } from 'react-router'
import { vi } from 'vitest'

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
      <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...result, client }
}

export interface StubbedCall {
  method: string
  url: string
  headers: Record<string, string>
  body: unknown
}

export interface StubRoute {
  method?: string
  /** Matched against the request URL: exact string, or a regex. */
  path: string | RegExp
  status?: number
  body?: unknown
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
export function stubFetch(routes: StubRoute[]) {
  const calls: StubbedCall[] = []
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const method = init.method ?? 'GET'
    calls.push({
      method,
      url,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: init.body === undefined ? undefined : JSON.parse(init.body as string),
    })
    const route = routes.find(
      (r) =>
        (r.method ?? 'GET') === method && (typeof r.path === 'string' ? r.path === url : r.path.test(url)),
    )
    if (!route) throw new Error(`unstubbed request: ${method} ${url}`)
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
