// @vitest-environment jsdom

import { act, screen, waitFor } from '@testing-library/react'
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

/** The layout's job, reduced to what this screen needs to run. */
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

const card = () => screen.getByTestId('file-list').closest('section')!
const select = (name: string) => userEvent.click(screen.getByRole('checkbox', { name: `Select ${name}` }))

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the command band', () => {
  it('is the same element before and after a selection, in the same place', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    // The node ITSELF, captured up front. Asserting that "a band exists" after
    // selecting would pass for a band that is mounted by the selection — which
    // is the bug: mounting one pushes the list down by its own height and the
    // row you are aiming at walks out from under the pointer.
    const band = screen.getByTestId('command-band')
    const listCard = card()
    expect(listCard.previousElementSibling).toBe(band)

    await select('notes.txt')

    expect(screen.getByRole('button', { name: 'Trash' })).toBeTruthy()
    expect(band.isConnected).toBe(true)
    expect(screen.getByTestId('command-band')).toBe(band)
    // Same card, too: a remount of either would put a different element here.
    expect(card()).toBe(listCard)
    expect(card().previousElementSibling).toBe(band)
  })

  it('offers no command until something is selected', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    // Queried by role, which skips what is `aria-hidden` or invisible: the
    // idle band is not a band with a greyed-out Trash button, it is a band
    // whose commands cannot be reached by a pointer, a tab, or a screen reader.
    expect(screen.queryByRole('button', { name: 'Trash' })).toBeNull()
    expect(screen.queryByRole('toolbar', { name: 'Selection actions' })).toBeNull()

    await select('notes.txt')

    expect(screen.getByRole('toolbar', { name: 'Selection actions' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Trash' })).toBeTruthy()
  })

  it('trashes every selected row, one request each', async () => {
    const { calls } = renderFolder([
      ...twoRows,
      { method: 'DELETE', path: '/api/nodes/f1', status: 204 },
      { method: 'DELETE', path: '/api/nodes/f2', status: 204 },
    ])
    await screen.findByText('notes.txt')

    await select('Reports')
    await select('notes.txt')
    await userEvent.click(screen.getByRole('button', { name: 'Trash' }))

    await waitFor(() => {
      const deletes = calls.filter((c) => c.method === 'DELETE')
      expect(deletes.map((c) => c.url).sort()).toEqual(['/api/nodes/f1', '/api/nodes/f2'])
    })
  })

  it('leaves the page interactive after a row menu hands off to a dialog (Radix pointer-events)', async () => {
    renderFolder(twoRows)
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    // A non-modal menu must not take the page away from the pointer at all —
    // the rows underneath it are drop targets the whole time it is open.
    expect(document.body.style.pointerEvents).not.toBe('none')

    await userEvent.click(await screen.findByRole('menuitem', { name: 'Rename' }))
    await screen.findByRole('dialog')
    // The dialog is modal, so it does take it, and has to give it back — with
    // the menu unmounting underneath it as it opens, which is the exact shape
    // this app regressed on once before.
    expect(document.body.style.pointerEvents).toBe('none')

    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    // Radix restores this from a queued task, so the assertion waits for one
    // to have run rather than reading the style in the same tick.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50))
    })
    expect(document.body.style.pointerEvents).not.toBe('none')
  })
})
