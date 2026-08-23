// @vitest-environment jsdom

import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useParams } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../FolderPage'

const user: Me = {
  id: 'u1',
  email: 'someone@example.test',
  display_name: 'Someone',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
}

const folder = (over: Partial<DriveNode>): DriveNode => ({
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

const root: DriveNode = folder({ id: 'root-1', parent_id: null, name: 'root' })

/**
 * Stands in for the app layout, which is what provides the current folder in
 * the product — the drop zone reads it from there rather than from a prop. The
 * seam itself (that the layout's value really is the route's `:id`) is pinned
 * in `app/__tests__/shell.test.tsx`; this is only what the screen needs to run.
 */
function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
    </CurrentFolderProvider>
  )
}

function renderFolder(routes: StubRoute[], { route = '/' } = {}) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
        <Route path="/folders/:id" element={<FolderPage />} />
      </Route>
    </Routes>,
    { route, seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('file browser', () => {
  it('lists a folder, points a file name at the viewer, and keeps Download on the menu', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      {
        path: '/api/nodes/root-1/children',
        body: {
          items: [
            folder({ id: 'f1', name: 'Reports' }),
            folder({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048, mime: 'text/plain' }),
          ],
          next_cursor: null,
        },
      },
    ])

    expect(await screen.findByRole('link', { name: 'Reports' })).toHaveProperty(
      'pathname',
      '/folders/f1',
    )
    expect(screen.getByText('2.0 KB')).toBeTruthy()
    // The name is the affordance that opens a file, and what it opens is the
    // viewer on the route already on screen — one parameter, so the preview is
    // linkable and the back button closes it.
    expect(screen.getByRole('link', { name: 'notes.txt' }).getAttribute('href')).toBe('/?preview=f2')
    // Downloading did not go anywhere; it moved to the row's own menu. Still a
    // navigation to the 302 and never a fetch, because the bytes come from the
    // object store and must not pass through this app.
    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    const download = await screen.findByRole('menuitem', { name: 'Download' })
    expect(download.getAttribute('href')).toBe('/api/files/f2/download')
  })

  it('walks the parent chain for breadcrumbs on a deep entry', async () => {
    renderFolder(
      [
        { path: '/api/nodes/root-1', body: root },
        { path: '/api/nodes/f1', body: folder({ id: 'f1', name: 'Reports', parent_id: 'root-1' }) },
        { path: '/api/nodes/f1/children', body: { items: [], next_cursor: null } },
      ],
      { route: '/folders/f1' },
    )

    const nav = await screen.findByRole('navigation', { name: /breadcrumb/i })
    // The root is labelled, not named after the stored folder row.
    expect(await within(nav).findByRole('link', { name: 'My Drive' })).toBeTruthy()
    expect(within(nav).getByText('Reports')).toBeTruthy()
    expect(await screen.findByText('This folder is empty.')).toBeTruthy()
  })

  it('trashes a row and refetches the folder', async () => {
    const { calls } = renderFolder([
      { path: '/api/nodes/root-1', body: root },
      {
        path: '/api/nodes/root-1/children',
        body: { items: [folder({ id: 'f2', kind: 'file', name: 'notes.txt', size: 10 })], next_cursor: null },
      },
      { method: 'DELETE', path: '/api/nodes/f2', status: 204 },
    ])

    await screen.findByText('notes.txt')
    // The row's own commands live behind its kebab now, rather than in a pair
    // of buttons repeated on every row.
    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Move to trash' }))

    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE')
      expect(del?.url).toBe('/api/nodes/f2')
      expect(del?.headers.get('X-Drive-Client')).toBe('web')
    })
    // The list is re-read rather than patched in place, so the row cannot
    // linger after the server has moved it to the trash.
    await waitFor(() =>
      expect(calls.filter((c) => c.url === '/api/nodes/root-1/children').length).toBeGreaterThan(1),
    )
  })

  // New folder — and with it the Radix `pointer-events` guard, which now has a
  // menu handing off to the dialog in front of it — moved to the rail with the
  // rest of the commands. Both cases live in `app/__tests__/shell.test.tsx`.
})

describe('selection and the actions it unlocks', () => {
  const twoRows: StubRoute[] = [
    { path: '/api/nodes/root-1', body: root },
    {
      path: '/api/nodes/root-1/children',
      body: {
        items: [
          folder({ id: 'f1', name: 'Reports' }),
          folder({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048 }),
        ],
        next_cursor: null,
      },
    },
  ]

  it('offers only the actions the selection can actually carry out', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    // Nothing selected: the band is mounted (it always is) but offers nothing —
    // its selected layer is out of the accessibility tree and out of reach.
    expect(screen.queryByRole('toolbar', { name: 'Selection actions' })).toBeNull()

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    const bar = () => screen.getByRole('toolbar', { name: 'Selection actions' })
    expect(within(bar()).getByText('1 selected')).toBeTruthy()
    // One file: everything is available.
    expect(within(bar()).getByRole('button', { name: 'Rename' })).toBeTruthy()
    expect(within(bar()).getByRole('link', { name: 'Download' })).toBeTruthy()
    expect(within(bar()).getByRole('button', { name: 'Move to' })).toBeTruthy()
    expect(within(bar()).getByRole('button', { name: 'Copy to' })).toBeTruthy()

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Reports' }))
    expect(within(bar()).getByText('2 selected')).toBeTruthy()
    // Rename takes exactly one item, and there is no archive endpoint to
    // download two things with, so neither is offered for a multi-selection.
    expect(within(bar()).queryByRole('button', { name: 'Rename' })).toBeNull()
    expect(within(bar()).queryByRole('link', { name: 'Download' })).toBeNull()
    // Copy survives, because one of the two is a file.
    expect(within(bar()).getByRole('button', { name: 'Copy to' })).toBeTruthy()
  })

  it('renames through the same endpoint a move uses, and re-reads the folder', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'PATCH', path: '/api/nodes/f2', body: folder({ id: 'f2', kind: 'file', name: 'renamed.txt' }) },
    ])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    await userEvent.click(
      within(screen.getByRole('toolbar', { name: 'Selection actions' })).getByRole('button', { name: 'Rename' }),
    )
    const field = screen.getByLabelText('Name')
    await userEvent.clear(field)
    await userEvent.type(field, 'renamed.txt')
    await userEvent.click(screen.getByRole('button', { name: 'Rename' }))

    await waitFor(() => {
      const patch = calls.find((c) => c.method === 'PATCH')
      expect(patch?.url).toBe('/api/nodes/f2')
      expect(patch!.body).toEqual({ name: 'renamed.txt' })
    })
    // The listing is re-read: a rename that only changed the local copy would
    // disagree with the server the moment anything else refetched.
    await waitFor(() => expect(calls.filter((c) => c.url === '/api/nodes/root-1/children').length).toBeGreaterThan(1))
  })

  it('moves a dragged row into the folder it was dropped on', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'PATCH', path: '/api/nodes/f2', body: folder({ id: 'f2', kind: 'file', name: 'notes.txt' }) },
    ])
    await screen.findByText('notes.txt')

    // Each row is also a right-click menu's trigger now. Radix wraps it in
    // place rather than around it, so the row is still the draggable element —
    // which is what these two tests keep honest. (Firefox's own drag under a
    // ContextMenu is a manual check; jsdom cannot answer it.)
    const rows = screen.getAllByRole('listitem')
    const fileRow = rows.find((r) => within(r).queryByText('notes.txt'))!
    const folderRow = rows.find((r) => within(r).queryByText('Reports'))!

    const payload = new Map<string, string>()
    const dataTransfer = {
      types: ['application/x-drive-node'],
      effectAllowed: '',
      dropEffect: '',
      setData: (type: string, value: string) => payload.set(type, value),
      getData: (type: string) => payload.get(type) ?? '',
    }

    fireEvent.dragStart(fileRow, { dataTransfer })
    fireEvent.dragOver(folderRow, { dataTransfer })
    fireEvent.drop(folderRow, { dataTransfer })

    await waitFor(() => {
      const patch = calls.find((c) => c.method === 'PATCH')
      expect(patch?.url).toBe('/api/nodes/f2')
      // parent_id, not name: this is a move, and the row carried its own id
      // even though nothing was selected before the drag started.
      expect(patch!.body).toEqual({ parent_id: 'f1' })
    })
  })

  it('will not drop a folder onto itself', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'PATCH', path: '/api/nodes/f2', body: folder({ id: 'f2', kind: 'file', name: 'notes.txt' }) },
    ])
    await screen.findByText('Reports')

    const rows = screen.getAllByRole('listitem')
    const fileRow = rows.find((r) => within(r).queryByText('notes.txt'))!
    const folderRow = rows.find((r) => within(r).queryByText('Reports'))!
    const payload = new Map<string, string>()
    const dataTransfer = {
      types: ['application/x-drive-node'],
      effectAllowed: '',
      dropEffect: '',
      setData: (type: string, value: string) => payload.set(type, value),
      getData: (type: string) => payload.get(type) ?? '',
    }

    // Reports onto Reports: a move that would make the folder its own parent.
    fireEvent.dragStart(folderRow, { dataTransfer })
    fireEvent.drop(folderRow, { dataTransfer })

    // Then a drop that IS legal. Asserting the absence of a request on its own
    // would pass before any request could have been made; making the legal
    // move the only PATCH is what actually pins the guard.
    fireEvent.dragStart(fileRow, { dataTransfer })
    fireEvent.drop(folderRow, { dataTransfer })

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    expect(calls.find((c) => c.method === 'PATCH')!.url).toBe('/api/nodes/f2')
  })
})
