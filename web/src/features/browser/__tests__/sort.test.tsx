// @vitest-environment jsdom

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useLocation, useParams } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../FolderPage'

/**
 * Sorting a folder from its column headers.
 *
 * The sort lives in the address rather than in component state, which is what
 * makes a sorted folder a place: it survives a reload, it is in the back
 * button, and it is what gets pasted to someone else. So both halves are under
 * test — that a click writes the address, and that the address is what the
 * request is built from.
 */

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
  kind: 'folder',
  name: 'Reports',
  size: null,
  mime: null,
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
  ...over,
})

const root = node({ id: 'root-1', parent_id: null, name: 'root' })

const items = {
  items: [
    node({ id: 'f1', name: 'Reports' }),
    node({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048 }),
  ],
  next_cursor: null,
}

function Where() {
  return <span data-testid="where">{useLocation().search}</span>
}

function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
      <Where />
    </CurrentFolderProvider>
  )
}

function renderFolder(routes: StubRoute[], route = '/') {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
      </Route>
    </Routes>,
    { route, seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

/** Every listing of this folder, however it is sorted. */
const anyListing: StubRoute = { path: /^\/api\/nodes\/root-1\/children/, body: items }
const listings = (calls: { url: string }[]) => calls.filter((c) => c.url.startsWith('/api/nodes/root-1/children'))

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('sorting from the column headers', () => {
  it('puts the column in the address and asks the server for it', async () => {
    const { calls } = renderFolder([{ path: '/api/nodes/root-1', body: root }, anyListing])
    await screen.findByText('notes.txt')

    // Nothing in the address to start with: the default is the server's, and
    // sending it anyway would make the plain folder URL a different cache
    // entry from the sorted-by-name one.
    expect(listings(calls)[0].url).toBe('/api/nodes/root-1/children')

    await userEvent.click(screen.getByRole('button', { name: 'Size' }))

    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('?sort=size&dir=asc'))
    await waitFor(() => {
      const last = listings(calls).at(-1)!.url
      expect(last).toContain('sort=size')
      expect(last).toContain('dir=asc')
    })

    // The same header again is the other direction — a sort control with no
    // way back to descending is half a control.
    await userEvent.click(screen.getByRole('button', { name: 'Size' }))

    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('?sort=size&dir=desc'))
    await waitFor(() => expect(listings(calls).at(-1)!.url).toContain('dir=desc'))
  })

  it('builds the first request from the address, not from a default', async () => {
    // A reload, a bookmark, or a link from someone else: the folder has to
    // arrive sorted the way the URL says, without a click.
    const { calls } = renderFolder(
      [{ path: '/api/nodes/root-1', body: root }, anyListing],
      '/?sort=updated_at&dir=desc',
    )
    await screen.findByText('notes.txt')

    const first = listings(calls)[0].url
    expect(first).toContain('sort=updated_at')
    expect(first).toContain('dir=desc')
  })
})
