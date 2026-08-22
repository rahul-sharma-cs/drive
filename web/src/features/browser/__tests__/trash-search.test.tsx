// @vitest-environment jsdom

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch } from '../../../test/render'
import { useLocation } from 'react-router'

import { HeaderSearch } from '../../../app/HeaderSearch'
import { meKey } from '../../auth/session'
import { SearchPage } from '../SearchPage'
import { TrashPage } from '../TrashPage'

// Search results now carry the same commands a folder's rows do, and a
// destination picker has to start from this account's root.
const user: Me = {
  id: 'u1',
  email: 'someone@example.test',
  display_name: 'Someone',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
}

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

/** The trash's two commands live on the row's kebab and its right-click menu. */
async function rowCommand(rowName: string, command: string) {
  await userEvent.click(screen.getByRole('button', { name: `Actions for ${rowName}` }))
  await userEvent.click(await screen.findByRole('menuitem', { name: command }))
}

describe('trash', () => {
  it('restores a trashed node, then re-reads the trash and every folder listing', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: [node({ id: 't1', name: 'old.txt' })], next_cursor: null } },
      { method: 'POST', path: '/api/trash/restore', body: { results: [{ id: 't1', status: 'ok' }], remaining: false } },
    ])
    const { client } = renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    const invalidate = vi.spyOn(client, 'invalidateQueries')
    await rowCommand('old.txt', 'Restore')

    // One row is a selection of one: the bulk route is the only path, so a
    // conflict on a single restore is reported the same way as on twenty.
    await waitFor(() => {
      const call = calls.find((c) => c.url === '/api/trash/restore')
      expect(call?.body).toEqual({ ids: ['t1'] })
    })
    await waitFor(() => expect(calls.filter((c) => c.url === '/api/trash').length).toBeGreaterThan(1))
    // The restored node reappears inside a folder, so those listings are stale
    // too. This screen renders none of them, so the invalidation is the only
    // observable — asserting only the /api/trash refetch would pass with it gone.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children'] }))
    // Same argument for an open search view: the restored node belongs in
    // those results again, and nothing else re-reads them.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['search'] }))
  })

  it('purges the one row through the purge route, never by emptying the trash', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: [node({ id: 't1', name: 'old.txt' })], next_cursor: null } },
      { method: 'POST', path: '/api/trash/purge', body: { results: [{ id: 't1', status: 'ok' }], remaining: false } },
    ])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    await rowCommand('old.txt', 'Delete forever')

    await waitFor(() => {
      const call = calls.find((c) => c.url === '/api/trash/purge')
      expect(call?.body).toEqual({ ids: ['t1'] })
    })
    // One row's Delete forever and the whole trash's Empty are one word apart
    // and worlds apart in consequence.
    expect(calls.some((c) => c.method === 'DELETE' && c.url === '/api/trash')).toBe(false)
  })

  it('says the trash is empty rather than showing a blank panel', async () => {
    stubFetch([{ path: '/api/trash', body: { items: [], next_cursor: null } }])
    renderApp(<TrashPage />)

    expect(await screen.findByText('The trash is empty.')).toBeTruthy()
  })
})

/** Reports the router's location, which is where the query has to end up. */
function Where() {
  return <span data-testid="where">{useLocation().search}</span>
}

describe('search', () => {
  // The box lives in the chrome and the results live on the screen; rendering
  // both is the only way to test what a person actually does, because the
  // query travels between them through the URL.
  const searchUI = (
    <>
      <HeaderSearch />
      <SearchPage />
      <Where />
    </>
  )

  it('queries once the typing settles and lists what matched', async () => {
    const calls = stubFetch([
      { path: /\/api\/search\?q=/, body: { items: [node({ id: 's1', name: 'report.pdf' })], next_cursor: null } },
    ])
    renderApp(searchUI, { seed: (client) => client.setQueryData(meKey, user) })

    await userEvent.type(screen.getByLabelText(/search by name/i), 'report')

    expect(await screen.findByText('report.pdf')).toBeTruthy()
    // Debounced: six keystrokes must not be six ILIKE queries.
    expect(calls.length).toBeLessThan(6)
    expect(calls[calls.length - 1].url).toBe('/api/search?q=report')
    // The name is what opens a file, and here it opens the viewer over the
    // results — carrying the query with it, because the query is what the list
    // underneath is made of.
    expect(screen.getByRole('link', { name: 'report.pdf' }).getAttribute('href')).toBe(
      '/search?q=report&preview=s1',
    )
  })

  it('puts the query in the URL, so a search is a place and not a mode', async () => {
    stubFetch([{ path: /\/api\/search\?q=/, body: { items: [], next_cursor: null } }])
    renderApp(searchUI, { seed: (client) => client.setQueryData(meKey, user) })

    await userEvent.type(screen.getByLabelText(/search by name/i), 'invoice')

    // MemoryRouter, so the location is read through the router rather than
    // through window.location.
    await waitFor(() => expect(screen.getByTestId('where').textContent).toContain('q=invoice'))
    // And the screen reads it from there rather than from its own state.
    expect(await screen.findByText(/Nothing matches/)).toBeTruthy()
  })

  it('asks for nothing until something is typed', async () => {
    const calls = stubFetch([])
    renderApp(searchUI, { seed: (client) => client.setQueryData(meKey, user) })

    await waitFor(() => expect(screen.getByLabelText(/search by name/i)).toBeTruthy())
    expect(calls).toHaveLength(0)
  })
})
