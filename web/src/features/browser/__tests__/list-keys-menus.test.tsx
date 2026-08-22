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

/**
 * The list's keys against the menus that open on top of it.
 *
 * A row menu is portalled to `document.body`, but React sends synthetic events
 * along the COMPONENT tree, so a keydown inside the menu still arrives at the
 * `<ul onKeyDown>` the menu's row belongs to. That is how Delete inside an open
 * kebab used to trash the selection behind it, Enter on *Rename* used to open
 * the rename dialog and the file's download tab at once, and Esc used to
 * dismiss the menu and clear the selection in the same press.
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

function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
    </CurrentFolderProvider>
  )
}

function renderFolder(routes: StubRoute[]) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route element={<FolderContext />}>
        <Route path="/" element={<FolderPage />} />
      </Route>
    </Routes>,
    { seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const rows = () => screen.getAllByTestId('file-row')
const rowFor = (name: string) => rows().find((r) => within(r).queryByText(name))!
const checked = (name: string) =>
  screen.getByRole('checkbox', { name: `Select ${name}` }).getAttribute('aria-checked')
const select = (name: string) => userEvent.click(screen.getByRole('checkbox', { name: `Select ${name}` }))

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('keys under an open row menu', () => {
  it('leaves the selection alone when Delete is pressed inside the kebab', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'DELETE', path: '/api/nodes/f1', status: 204 },
      { method: 'DELETE', path: '/api/nodes/f2', status: 204 },
    ])
    await screen.findByText('notes.txt')

    await select('Reports')
    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    await screen.findByRole('menu')

    await userEvent.keyboard('{Delete}')

    // Nothing on screen said "delete these rows": the menu is what has focus,
    // and the selection behind it is not even visible under it.
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)
    expect(checked('Reports')).toBe('true')
  })

  it('opens only the rename dialog when Enter chooses Rename', async () => {
    const open = vi.fn()
    vi.stubGlobal('open', open)
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    await screen.findByRole('menu')
    // Down to Rename — the arrows that walk the menu used to walk the list's
    // own roving focus at the same time, which is what gave Enter a second
    // meaning.
    await userEvent.keyboard('{ArrowDown}{ArrowDown}')
    expect(screen.getByRole('menuitem', { name: 'Rename' })).toBe(document.activeElement)
    await userEvent.keyboard('{Enter}')

    const field = await screen.findByLabelText('Name')
    expect((field as HTMLInputElement).value).toBe('notes.txt')
    // The row under the menu is a file, and opening it means a download tab.
    expect(open).not.toHaveBeenCalled()
  })

  it('keeps the selection when Escape dismisses the right-click menu', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    await select('Reports')
    fireEvent.contextMenu(rowFor('Reports'))
    await screen.findByRole('menu')

    await userEvent.keyboard('{Escape}')

    // Escape dismissed the menu. Dismissing a menu is not a decision about the
    // rows behind it.
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull())
    expect(checked('Reports')).toBe('true')
  })
})
