// @vitest-environment jsdom

import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useParams } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { meKey } from '../../auth/session'
import { FolderPage } from '../FolderPage'
import { TrashPage } from '../TrashPage'

/**
 * What the list does with the states around the rows: loading, empty, failed,
 * and — the one that used to move things — a refetch that fails while good rows
 * are still on screen.
 *
 * The query keeps its last successful data through a failed refetch, so the
 * rows are still true. Putting an error block inside the card under those
 * circumstances pushed every row down by the height of the block, for a failure
 * that changed nothing on screen.
 */

const errorToast = vi.fn()
vi.mock('sonner', () => ({
  toast: {
    error: (...args: unknown[]) => errorToast(...args),
    success: vi.fn(),
  },
}))

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
  items: [node({ id: 'f1', name: 'Reports' }), node({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048 })],
  next_cursor: null,
}

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
const band = () => screen.getByTestId('command-band')

beforeEach(() => {
  errorToast.mockClear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('a refetch that fails', () => {
  it('leaves the rows exactly where they were and says so out of the flow', async () => {
    // The route object is mutated between the two answers, so the same query
    // succeeds first and fails second — which is the situation, not a first
    // load that never had rows.
    const children: StubRoute = { path: '/api/nodes/root-1/children', body: items }
    const { client } = renderFolder([{ path: '/api/nodes/root-1', body: root }, children])
    await screen.findByText('notes.txt')

    // The elements themselves, captured before the failure. Asserting that
    // "a row exists" afterwards would pass for a list that had been pushed
    // down the page by an error block appearing above it.
    const firstRow = screen.getAllByTestId('file-row')[0]
    const listCard = card()
    const bandBefore = band()
    expect(listCard.previousElementSibling).toBe(bandBefore)

    children.status = 500
    children.body = { code: 'internal', message: 'the folder didn’t load' }
    await act(async () => {
      await client.invalidateQueries({ queryKey: ['children'] })
    })

    await waitFor(() => expect(errorToast).toHaveBeenCalledTimes(1))
    expect(firstRow.isConnected).toBe(true)
    expect(screen.getAllByTestId('file-row')[0]).toBe(firstRow)
    expect(card()).toBe(listCard)
    expect(band()).toBe(bandBefore)
    expect(listCard.previousElementSibling).toBe(bandBefore)
    // Nothing was inserted into the card above the rows.
    expect(within(listCard).queryByRole('alert')).toBeNull()
  })

  it('still replaces the rows when there are none to keep', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', status: 500, body: { code: 'internal', message: 'no' } },
    ])

    // Nothing is on screen to protect, so the failure belongs where the rows
    // would have been, with the retry beside it.
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain('didn’t load')
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
    expect(screen.queryByTestId('file-row')).toBeNull()
    expect(errorToast).not.toHaveBeenCalled()
  })

  it('keeps the trash’s rows in place when a restore fails', async () => {
    stubFetch([
      { path: '/api/trash', body: { items: [node({ id: 't1', kind: 'file', name: 'old.txt' })], next_cursor: null } },
      { method: 'POST', path: '/api/nodes/t1/restore', status: 409, body: { code: 'name_conflict', message: 'taken' } },
    ])
    renderApp(<TrashPage />)
    await screen.findByText('old.txt')

    const firstRow = screen.getAllByTestId('file-row')[0]
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    await waitFor(() => expect(errorToast).toHaveBeenCalled())
    // A message in the flow above the list moves every row in it, which is the
    // same shift the command band exists to prevent.
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getAllByTestId('file-row')[0]).toBe(firstRow)
  })
})

describe('the band', () => {
  const noRows = { items: [], next_cursor: null }

  it('is mounted while the first page is still in flight', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', body: items },
    ])

    // Before the answer arrives — the skeleton state. A band that waited for
    // rows would arrive underneath the pointer along with them.
    expect(band()).toBeTruthy()
    await screen.findByText('notes.txt')
    expect(band()).toBeTruthy()
  })

  it('is mounted on an empty folder', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', body: noRows },
    ])

    await screen.findByText('This folder is empty.')
    expect(band()).toBeTruthy()
  })

  it('is mounted on a folder that failed to load', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', status: 500, body: { code: 'internal', message: 'no' } },
    ])

    await screen.findByRole('alert')
    expect(band()).toBeTruthy()
  })
})

describe('focus', () => {
  it('comes back to the list when the layer holding it is hidden', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', body: items },
    ])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    await userEvent.click(screen.getByRole('button', { name: 'Clear the selection' }))

    // The ✕ that was just clicked sits in a layer that is now `aria-hidden` and
    // invisible. Leaving focus there is what makes Chrome log "Blocked
    // aria-hidden on an element because its descendant retained focus", and it
    // strands a keyboard user on a control nothing can see.
    const active = document.activeElement as HTMLElement
    expect(active.closest('[aria-hidden="true"]')).toBeNull()
    expect(screen.getByTestId('file-list').contains(active)).toBe(true)
  })
})
