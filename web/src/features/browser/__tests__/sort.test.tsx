// @vitest-environment jsdom

import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useLocation, useParams } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../FolderPage'

/**
 * Sorting a folder, and what a folder row says instead of a size.
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

/** A folder that knows how much it holds, and a file that knows how big it is. */
const items = {
  items: [
    node({ id: 'f1', name: 'Reports', item_count: 3 }),
    node({ id: 'f3', name: 'Empty', item_count: 0 }),
    // `item_count` on a file as well, so that a cell which stopped caring what
    // kind of row it is on has something to get wrong.
    node({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048, item_count: 9 }),
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

const rowFor = (name: string) => screen.getAllByTestId('file-row').find((r) => within(r).queryByText(name))!

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

    // Nothing is sorted by size yet, so the header is named plainly.
    await userEvent.click(screen.getByRole('button', { name: 'Size' }))

    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('?sort=size&dir=asc'))
    await waitFor(() => {
      const last = listings(calls).at(-1)!.url
      expect(last).toContain('sort=size')
      expect(last).toContain('dir=asc')
    })

    // The arrow that appeared is the whole of the state on screen, and an arrow
    // is not something a screen reader can read. So the name carries it — and
    // only on the column in force: the other two are not sorted in any
    // direction, and naming one of them would be a claim about an order the
    // list is not in.
    expect(await screen.findByRole('button', { name: 'Size, ascending' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Name' })).toBeTruthy()

    // The same header again is the other direction — a sort control with no
    // way back to descending is half a control.
    await userEvent.click(screen.getByRole('button', { name: 'Size, ascending' }))

    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('?sort=size&dir=desc'))
    await waitFor(() => expect(listings(calls).at(-1)!.url).toContain('dir=desc'))
    expect(await screen.findByRole('button', { name: 'Size, descending' })).toBeTruthy()
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
    // And the column says so without a click having happened at all.
    expect(screen.getByRole('button', { name: 'Modified, descending' })).toBeTruthy()
  })
})

describe('what a folder row shows instead of a size', () => {
  it('counts what a folder holds, and leaves a file to its bytes', async () => {
    renderFolder([{ path: '/api/nodes/root-1', body: root }, anyListing])
    await screen.findByText('notes.txt')

    expect(within(rowFor('Reports')).getByText('3 items')).toBeTruthy()
    // Zero is a count the server sends, not a missing one: an empty folder
    // says so, and only a cell testing the field for truth would blank it.
    expect(within(rowFor('Empty')).getByText('0 items')).toBeTruthy()
    // The file carries a count too, and must ignore it: a size column that
    // reported "9 items" for a text file would be nonsense no test that only
    // looked at folders would notice.
    expect(within(rowFor('notes.txt')).getByText('2.0 KB')).toBeTruthy()
    expect(within(rowFor('notes.txt')).queryByText(/item/)).toBeNull()
  })

  it('says nothing where the listing does not count', async () => {
    // Search results and the trash send no count. An empty cell is right
    // there; "0 items" would be a claim the answer never made.
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: /^\/api\/nodes\/root-1\/children/, body: { items: [node({ id: 'f1', name: 'Reports' })], next_cursor: null } },
    ])
    await screen.findByText('Reports')

    expect(within(rowFor('Reports')).queryByText(/item/)).toBeNull()
  })
})
