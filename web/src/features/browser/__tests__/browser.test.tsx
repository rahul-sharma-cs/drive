// @vitest-environment jsdom

import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Route, Routes } from 'react-router'

import type { DriveNode, Me } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
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

function renderFolder(routes: StubRoute[], { route = '/' } = {}) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <Routes>
      <Route path="/" element={<FolderPage />} />
      <Route path="/folders/:id" element={<FolderPage />} />
    </Routes>,
    { route, seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('file browser', () => {
  it('lists a folder and links a file at the download endpoint', async () => {
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
    expect(screen.getByText('notes.txt')).toBeTruthy()
    expect(screen.getByText('2.0 KiB')).toBeTruthy()
    // Download is a navigation to the 302, never a fetch: the bytes come from
    // the object store directly and must not pass through this app.
    const download = screen.getByRole('link', { name: 'Download' })
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
    await userEvent.click(screen.getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE')
      expect(del?.url).toBe('/api/nodes/f2')
      expect(del?.headers['X-Drive-Client']).toBe('web')
    })
    // The list is re-read rather than patched in place, so the row cannot
    // linger after the server has moved it to the trash.
    await waitFor(() =>
      expect(calls.filter((c) => c.url === '/api/nodes/root-1/children').length).toBeGreaterThan(1),
    )
  })

  it('creates a folder from the dialog and re-reads the folder afterwards', async () => {
    const { calls } = renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', body: { items: [], next_cursor: null } },
      { method: 'POST', path: '/api/folders', body: folder({ id: 'new-1', name: 'Invoices' }) },
    ])

    await screen.findByText('This folder is empty.')
    await userEvent.click(screen.getByRole('button', { name: 'New folder' }))
    await userEvent.type(await screen.findByLabelText('Name'), 'Invoices')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST')
      expect(post?.url).toBe('/api/folders')
      expect(post?.body).toEqual({ parent_id: 'root-1', name: 'Invoices' })
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() =>
      expect(calls.filter((c) => c.url === '/api/nodes/root-1/children').length).toBeGreaterThan(1),
    )
  })

  it('leaves the page interactive after the dialog closes (Radix pointer-events)', async () => {
    renderFolder([
      { path: '/api/nodes/root-1', body: root },
      { path: '/api/nodes/root-1/children', body: { items: [], next_cursor: null } },
    ])

    await screen.findByText('This folder is empty.')
    await userEvent.click(screen.getByRole('button', { name: 'New folder' }))
    await screen.findByRole('dialog')
    // Radix sets pointer-events:none on <body> while a modal is open, and a
    // stuck value there disables every drop target and button on the page.
    expect(document.body.style.pointerEvents).toBe('none')

    await userEvent.keyboard('{Escape}')

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => expect(document.body.style.pointerEvents).not.toBe('none'))
  })
})
