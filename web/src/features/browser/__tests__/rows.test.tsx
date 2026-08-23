// @vitest-environment jsdom

import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useLocation, useParams } from 'react-router'

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
  has_password: true,
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

const twoRows: StubRoute[] = [
  { path: '/api/nodes/root-1', body: root },
  {
    path: '/api/nodes/root-1/children',
    body: {
      items: [node({ id: 'f1', name: 'Reports' }), node({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048 })],
      next_cursor: null,
    },
  },
]

/** Reports the router's location, for the navigations a name link performs. */
function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>
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

function renderFolder(routes: StubRoute[]) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
        <Route path="/folders/:id" element={<FolderPage />} />
      </Route>
    </Routes>,
    { seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const rows = () => screen.getAllByTestId('file-row')
const rowFor = (name: string) => rows().find((r) => within(r).queryByText(name))!
const selecting = () => screen.queryByRole('toolbar', { name: 'Selection actions' })

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('rows', () => {
  it('takes every loaded row when the header checkbox is clicked, and says so while it is partial', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    const all = screen.getByRole('checkbox', { name: 'Select all 2 loaded' })
    expect(all.getAttribute('aria-checked')).toBe('false')

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    // Mixed is a third state and not an unchecked box: one of the two rows is
    // selected, and the header has to say "some of these".
    expect(all.getAttribute('aria-checked')).toBe('mixed')
    // And it says it with a dash. The mark in the box is the whole of what a
    // person reads here, and a check would say "all of them".
    expect(all.querySelector('svg.lucide-minus')).not.toBeNull()
    expect(all.querySelector('svg.lucide-check')).toBeNull()

    await userEvent.click(all)
    // The state is only half of it — what the click has to actually do is take
    // every row. Asserting the header's own state alone would pass with the
    // rows untouched.
    for (const row of rows()) {
      expect(within(row).getByRole('checkbox').getAttribute('aria-checked')).toBe('true')
    }
    expect(screen.getByRole('checkbox', { name: 'Select all 2 loaded' }).getAttribute('aria-checked')).toBe('true')
    // Whole selection, check back.
    expect(all.querySelector('svg.lucide-check')).not.toBeNull()
    expect(all.querySelector('svg.lucide-minus')).toBeNull()

    // And a selected row's own box still unselects it. The row's click handler
    // has to stand aside for a control inside it: a toggle followed by the
    // row's select-only in the same gesture would put the row straight back.
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    expect(screen.getByRole('checkbox', { name: 'Select notes.txt' }).getAttribute('aria-checked')).toBe('false')
    expect(screen.getByRole('checkbox', { name: 'Select Reports' }).getAttribute('aria-checked')).toBe('true')
  })

  it('renames the row that was right-clicked, not the one that happens to be selected', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    // Something else is selected: the menu must ignore it.
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Reports' }))

    fireEvent.contextMenu(rowFor('notes.txt'))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Rename' }))

    const field = await screen.findByLabelText('Name')
    expect((field as HTMLInputElement).value).toBe('notes.txt')
  })

  it('trashes the whole selection on Delete', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'DELETE', path: '/api/nodes/f1', status: 204 },
      { method: 'DELETE', path: '/api/nodes/f2', status: 204 },
    ])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Reports' }))
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    fireEvent.keyDown(screen.getByTestId('file-list'), { key: 'Delete' })

    await waitFor(() => {
      const deletes = calls.filter((c) => c.method === 'DELETE')
      expect(deletes.map((c) => c.url).sort()).toEqual(['/api/nodes/f1', '/api/nodes/f2'])
    })
  })

  it('opens from the name and selects from everywhere else on the row', async () => {
    renderFolder([
      ...twoRows,
      // The name opens the viewer, and the viewer asks for a link the moment it
      // does. What it answers with is another test's subject.
      { path: '/api/files/f2/preview', status: 415, body: { code: 'unsupported', message: 'no preview' } },
    ])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('link', { name: 'notes.txt' }))
    expect(await screen.findByRole('dialog')).toBeTruthy()
    // Opening a file is not selecting it.
    expect(selecting()).toBeNull()
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    await userEvent.click(within(rowFor('notes.txt')).getByText('2.0 KB'))
    expect(selecting()).toBeTruthy()
    expect(screen.getByRole('checkbox', { name: 'Select notes.txt' }).getAttribute('aria-checked')).toBe('true')
  })

  it('walks into a folder from its name', async () => {
    renderFolder([
      ...twoRows,
      { path: '/api/nodes/f1', body: node({ id: 'f1', name: 'Reports' }) },
      { path: '/api/nodes/f1/children', body: { items: [], next_cursor: null } },
    ])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('link', { name: 'Reports' }))

    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/folders/f1'))
  })
})
