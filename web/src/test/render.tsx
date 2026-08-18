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

export function renderApp(ui: ReactNode, { route = '/' }: { route?: string } = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  })
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

/**
 * Replaces global fetch with a queue of canned responses and records the calls.
 * Screens talk to the real `lib/api` client, so the assertions cover the wire
 * shape — the header the CSRF gate requires included.
 */
export function stubFetch(responses: Array<{ status: number; body: unknown }>) {
  const calls: StubbedCall[] = []
  const queue = [...responses]
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({
      method: init.method ?? 'GET',
      url,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: init.body === undefined ? undefined : JSON.parse(init.body as string),
    })
    const next = queue.shift()
    if (!next) throw new Error(`unexpected fetch: ${init.method} ${url}`)
    return new Response(next.body === undefined ? '' : JSON.stringify(next.body), {
      status: next.status,
      headers: { 'Content-Type': 'application/json' },
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return calls
}
