// @vitest-environment jsdom

/**
 * `/shared`: every link the account has out, with the dialog's own actions on
 * each row. The first case goes through the real route table and the rail;
 * the rest render the screen on its own.
 */

import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast, Toaster } from 'sonner'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Me, Share } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import App from '../../../App'
import { meKey } from '../../auth/session'
import { LINK_NOT_KEPT } from '../ShareDialog'
import { SharedLinksPage } from '../SharedLinksPage'
import { shareUrls } from '../shareUrls'

// The layout mounts the upload dock, whose real store drives the singleton
// engine — Web Workers and IndexedDB. What this file is about is the list.
vi.mock('../../upload/ui/engineStore', () => ({
  uploadActions: {
    enqueue: () => {},
    pause: () => {},
    resume: () => {},
    retry: () => {},
    cancel: () => {},
    reselect: () => {},
    resolveConflict: () => {},
    clearFinished: () => {},
  },
  useUploadItems: () => [],
}))

const user: Me = {
  id: 'u1',
  email: 'someone@example.test',
  display_name: 'Someone',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
  has_password: true,
}

const DAY = 24 * 60 * 60 * 1000
const URL_1 = 'https://drive.example/s/0123456789abcdef0123456789abcdef0123456789a'

const share = (over: Partial<Share> = {}): Share => ({
  id: 's1',
  node: { id: 'f2', parent_id: 'folder-9', name: 'notes.txt', size: 2048, mime: 'text/plain' },
  node_live: true,
  has_password: true,
  expires_at: new Date(Date.now() + 7 * DAY + 60_000).toISOString(),
  max_downloads: 2,
  download_count: 1,
  created_at: '2026-08-20T00:00:00Z',
  ...over,
})

const chrome: StubRoute[] = [
  { path: '/api/uploads', body: { items: [], next_cursor: null } },
  { path: '/api/usage', body: { used: 0, quota: 1_000_000 } },
]

function renderList(routes: StubRoute[]) {
  const calls = stubFetch(routes)
  const rendered = renderApp(
    <>
      <SharedLinksPage />
      <Toaster />
    </>,
    { route: '/shared', seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

const rows = () => screen.getAllByTestId('share-row')

afterEach(() => {
  vi.unstubAllGlobals()
  shareUrls.clear()
  toast.dismiss()
})

describe('getting there', () => {
  it('is a rail entry between My Drive and Trash, and a route inside the layout', async () => {
    stubFetch([...chrome, { path: '/api/shares', body: { items: [share()], next_cursor: null } }])
    renderApp(<App />, { route: '/shared', seed: (client) => client.setQueryData(meKey, user) })

    expect(await screen.findByRole('heading', { name: 'Shared links' })).toBeTruthy()
    const places = within(screen.getByRole('navigation', { name: 'Places' })).getAllByRole('link')
    expect(places.map((link) => link.textContent)).toEqual(['My Drive', 'Shared links', 'Trash'])
    expect(places[1].getAttribute('href')).toBe('/shared')
    expect(await screen.findByRole('link', { name: 'notes.txt' })).toBeTruthy()
  })
})

describe('the rows', () => {
  it('names the file into its folder with the viewer open, and says what stands in front of the link', async () => {
    renderList([
      {
        path: '/api/shares',
        body: {
          items: [share(), share({ id: 's2', node_live: false, has_password: false, max_downloads: null, download_count: 0 })],
          next_cursor: null,
        },
      },
    ])

    expect(await screen.findByRole('columnheader', { name: 'Expires' })).toBeTruthy()
    expect(screen.getByRole('columnheader', { name: 'Downloads' })).toBeTruthy()
    const [first, second] = rows()

    expect(within(first).getByRole('link', { name: 'notes.txt' }).getAttribute('href')).toBe(
      '/folders/folder-9?preview=f2',
    )
    expect(within(first).getByText('Expires in 7 days')).toBeTruthy()
    expect(within(first).getByText('1 of 2')).toBeTruthy()
    expect(within(first).getByText('Password')).toBeTruthy()
    expect(within(first).queryByText('In trash')).toBeNull()

    expect(within(second).getByText('In trash')).toBeTruthy()
    expect(within(second).queryByText('Password')).toBeNull()
    expect(within(second).getByText('0')).toBeTruthy()
  })

  it('is an invitation when there is nothing', async () => {
    renderList([{ path: '/api/shares', body: { items: [], next_cursor: null } }])

    expect(await screen.findByText('No share links yet.')).toBeTruthy()
    expect(screen.getByText('Share a file from its row menu and the link turns up here.')).toBeTruthy()
    expect(screen.queryByRole('table')).toBeNull()
  })

  it('pages with Load more rather than showing the first answer as the whole', async () => {
    const { calls } = renderList([
      { path: '/api/shares', body: { items: [share()], next_cursor: 'page-2' } },
      { path: '/api/shares?cursor=page-2', body: { items: [share({ id: 's2', node: { ...share().node, id: 'f3', name: 'two.txt' } })], next_cursor: null } },
    ])

    await screen.findByRole('link', { name: 'notes.txt' })
    expect(rows()).toHaveLength(1)
    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))

    expect(await screen.findByRole('link', { name: 'two.txt' })).toBeTruthy()
    expect(rows()).toHaveLength(2)
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull()
    expect(calls.some((c) => c.url === '/api/shares?cursor=page-2')).toBe(true)
  })
})

describe('the actions', () => {
  it('offers Copy only on a row whose URL this tab minted', async () => {
    shareUrls.set('s1', URL_1)
    renderList([
      { path: '/api/shares', body: { items: [share(), share({ id: 's2' })], next_cursor: null } },
    ])

    await screen.findAllByRole('link', { name: 'notes.txt' })
    const [held, other] = rows()
    expect((within(held).getByLabelText('Link') as HTMLInputElement).value).toBe(URL_1)
    expect(within(held).getByRole('button', { name: 'Copy link' })).toBeTruthy()
    expect(within(held).queryByText(LINK_NOT_KEPT)).toBeNull()
    expect(within(other).queryByRole('button', { name: 'Copy link' })).toBeNull()
    expect(within(other).getByText(LINK_NOT_KEPT)).toBeTruthy()
    expect(within(other).getByRole('button', { name: 'New link' })).toBeTruthy()
  })

  it('Settings opens the file’s own Share dialog, on that share, at its settings form', async () => {
    const { calls } = renderList([
      { path: '/api/shares', body: { items: [share(), share({ id: 's2', node: { ...share().node, id: 'f3', name: 'two.txt' } })], next_cursor: null } },
      { path: '/api/shares?node_id=f3', body: { items: [share({ id: 's2', max_downloads: 5, download_count: 0 })], next_cursor: null } },
    ])

    await screen.findAllByRole('link', { name: 'notes.txt' })
    await userEvent.click(within(rows()[1]).getByRole('button', { name: 'Settings' }))

    // The dialog the row menu opens, read from the server for this file.
    const dialog = await screen.findByRole('dialog', { name: 'Share "two.txt"' })
    expect(await within(dialog).findByText('Expires in 7 days · Password on · 0 of 5 downloads')).toBeTruthy()
    expect(calls.some((c) => c.url === '/api/shares?node_id=f3')).toBe(true)
    // No URL in this browser, and the dialog says so rather than offering Copy.
    expect(within(dialog).getByText(LINK_NOT_KEPT)).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Copy link' })).toBeNull()

    await userEvent.click(within(dialog).getByRole('button', { name: 'Settings' }))
    expect((within(dialog).getByLabelText('Download limit') as HTMLInputElement).value).toBe('5')
    expect(within(dialog).getByRole('button', { name: 'Save settings' })).toBeTruthy()

    // Closing it leaves the list where it was.
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Share "two.txt"' })).toBeNull())
    expect(rows()).toHaveLength(2)
  })

  it('New link asks, then regenerates and shows the new URL on the row', async () => {
    const URL_2 = 'https://drive.example/s/fedcba9876543210fedcba9876543210fedcba98765'
    const { calls } = renderList([
      { path: '/api/shares', body: { items: [share()], next_cursor: null } },
      { method: 'PATCH', path: '/api/shares/s1', body: { share: share(), url: URL_2 } },
    ])

    await screen.findByRole('link', { name: 'notes.txt' })
    await userEvent.click(within(rows()[0]).getByRole('button', { name: 'New link' }))
    const ask = await screen.findByRole('dialog', { name: 'Make a new link?' })
    expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(0)
    await userEvent.click(within(ask).getByRole('button', { name: 'New link' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    expect(calls.find((c) => c.method === 'PATCH')!.body).toEqual({ action: 'regenerate' })
    await waitFor(() => expect((within(rows()[0]).getByLabelText('Link') as HTMLInputElement).value).toBe(URL_2))
  })

  it('Stop sharing asks, then revokes and the row leaves', async () => {
    const list: StubRoute = { path: '/api/shares', body: { items: [share()], next_cursor: null } }
    const { calls } = renderList([list, { method: 'DELETE', path: '/api/shares/s1', status: 204 }])

    await screen.findByRole('link', { name: 'notes.txt' })
    await userEvent.click(within(rows()[0]).getByRole('button', { name: 'Stop sharing' }))
    const ask = await screen.findByRole('dialog', { name: 'Stop sharing?' })
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)

    list.body = { items: [], next_cursor: null }
    await userEvent.click(within(ask).getByRole('button', { name: 'Stop sharing' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1))
    expect(await screen.findByText('No share links yet.')).toBeTruthy()
    expect(await screen.findByText('Sharing stopped')).toBeTruthy()
  })
})
