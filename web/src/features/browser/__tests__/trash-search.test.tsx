// @vitest-environment jsdom

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { DriveNode } from '../../../lib/api'
import { renderApp, stubFetch } from '../../../test/render'
import { SearchPage } from '../SearchPage'
import { TrashPage } from '../TrashPage'

const node = (over: Partial<DriveNode>): DriveNode => ({
  id: 'n1',
  parent_id: 'root-1',
  kind: 'file',
  name: 'notes.txt',
  size: 12,
  mime: 'text/plain',
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
  ...over,
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('trash', () => {
  it('restores a trashed node, then re-reads the trash and every folder listing', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: [node({ id: 't1', name: 'old.txt' })], next_cursor: null } },
      { method: 'POST', path: '/api/nodes/t1/restore', body: node({ id: 't1' }) },
    ])
    const { client } = renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    const invalidate = vi.spyOn(client, 'invalidateQueries')
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    await waitFor(() => expect(calls.some((c) => c.url === '/api/nodes/t1/restore')).toBe(true))
    await waitFor(() => expect(calls.filter((c) => c.url === '/api/trash').length).toBeGreaterThan(1))
    // The restored node reappears inside a folder, so those listings are stale
    // too. This screen renders none of them, so the invalidation is the only
    // observable — asserting only the /api/trash refetch would pass with it gone.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children'] }))
    // Same argument for an open search view: the restored node belongs in
    // those results again, and nothing else re-reads them.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['search'] }))
  })

  it('purges for good through the purge endpoint, not the trash one', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: [node({ id: 't1', name: 'old.txt' })], next_cursor: null } },
      { method: 'DELETE', path: '/api/nodes/t1/purge', status: 204 },
    ])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    await userEvent.click(screen.getByRole('button', { name: 'Delete forever' }))

    await waitFor(() => {
      const call = calls.find((c) => c.method === 'DELETE')
      expect(call?.url).toBe('/api/nodes/t1/purge')
    })
  })

  it('says the trash is empty rather than showing a blank panel', async () => {
    stubFetch([{ path: '/api/trash', body: { items: [], next_cursor: null } }])
    renderApp(<TrashPage />)

    expect(await screen.findByText('The trash is empty.')).toBeTruthy()
  })
})

describe('search', () => {
  it('queries once the typing settles and lists what matched', async () => {
    const calls = stubFetch([
      { path: /\/api\/search\?q=/, body: { items: [node({ id: 's1', name: 'report.pdf' })], next_cursor: null } },
    ])
    renderApp(<SearchPage />)

    await userEvent.type(screen.getByLabelText(/search by name/i), 'report')

    expect(await screen.findByText('report.pdf')).toBeTruthy()
    // Debounced: six keystrokes must not be six ILIKE queries.
    expect(calls.length).toBeLessThan(6)
    expect(calls[calls.length - 1].url).toBe('/api/search?q=report')
    expect(screen.getByRole('link', { name: 'Download' }).getAttribute('href')).toBe('/api/files/s1/download')
  })

  it('asks for nothing until something is typed', async () => {
    const calls = stubFetch([])
    renderApp(<SearchPage />)

    await waitFor(() => expect(screen.getByLabelText(/search by name/i)).toBeTruthy())
    expect(calls).toHaveLength(0)
  })
})
