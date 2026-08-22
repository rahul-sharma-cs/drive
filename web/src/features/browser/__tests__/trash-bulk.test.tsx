// @vitest-environment jsdom

import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { DriveNode } from '../../../lib/api'
import { renderApp, stubFetch, type StubbedCall, type StubRoute } from '../../../test/render'
import { formatWhen } from '../../../ui/when'
import { TrashPage } from '../TrashPage'

/**
 * Emptying the trash, and doing things to more than one row at a time.
 *
 * The routes here answer 200 and report per id, so "it worked" is never what a
 * status code says — a restore that hit a name collision, and a call that ran
 * out of budget half way down the list, both come back as successes carrying
 * bad news. What this file pins is that the screen reads that news: it sends
 * one request for a selection rather than one per row, it keeps asking while
 * the server says there is more, and it says out loud which rows stayed put.
 */

const errorToast = vi.fn()
vi.mock('sonner', () => ({
  toast: {
    error: (...args: unknown[]) => errorToast(...args),
    success: vi.fn(),
  },
}))

const node = (over: Partial<DriveNode>): DriveNode => ({
  id: 'n1',
  parent_id: 'root-1',
  kind: 'file',
  name: 'notes.txt',
  size: 12,
  mime: 'text/plain',
  created_at: '2019-03-04T00:00:00Z',
  updated_at: '2019-03-04T00:00:00Z',
  deleted_at: '2021-11-09T00:00:00Z',
  ...over,
})

const two = [node({ id: 't1', name: 'old.txt' }), node({ id: 't2', name: 'older.txt' })]

const selectAll = () => userEvent.click(screen.getByRole('checkbox', { name: /^Select all/ }))

afterEach(() => {
  vi.unstubAllGlobals()
  errorToast.mockClear()
})

describe('the trash in bulk', () => {
  it('restores the whole selection in one request, and re-reads what that changed', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: two, next_cursor: null } },
      {
        method: 'POST',
        path: '/api/trash/restore',
        body: { results: two.map((n) => ({ id: n.id, status: 'ok' })), remaining: false },
      },
    ])
    const { client } = renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    const invalidate = vi.spyOn(client, 'invalidateQueries')
    await selectAll()
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    await waitFor(() => {
      const posts = calls.filter((c) => c.url === '/api/trash/restore')
      // One call carrying both ids, not one call per row: the route exists
      // precisely so that restoring fifty things is not fifty requests, and a
      // loop over `mutate` would pass any assertion that only counted ids.
      expect(posts).toHaveLength(1)
      expect(posts[0].body).toEqual({ ids: ['t1', 't2'] })
    })

    // The restored rows are back inside a folder and back in search results,
    // and this screen renders neither — so the invalidation is the only
    // observable. Asserting the trash refetch alone would pass without them.
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children'] }))
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['trash'] })
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['search'] })
  })

  it('deletes the whole selection for good on the Delete key', async () => {
    const calls = stubFetch([
      { path: '/api/trash', body: { items: two, next_cursor: null } },
      {
        method: 'POST',
        path: '/api/trash/purge',
        body: { results: two.map((n) => ({ id: n.id, status: 'ok' })), remaining: false },
      },
    ])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    await selectAll()
    // In a folder this key trashes. Here there is nowhere further down to go,
    // so it has to reach the purge route rather than quietly doing nothing.
    fireEvent.keyDown(screen.getByTestId('file-list'), { key: 'Delete' })

    await waitFor(() => {
      const posts = calls.filter((c) => c.url === '/api/trash/purge')
      expect(posts).toHaveLength(1)
      expect(posts[0].body).toEqual({ ids: ['t1', 't2'] })
    })
  })

  it('empties the trash after confirming the count, and keeps going while the server says there is more', async () => {
    // Two different answers from one route. The body is read at call time, so
    // a getter is what lets the second call see something different — the loop
    // runs inside the mutation and the test cannot step into the middle of it.
    // It counts requests rather than its own reads, which the harness makes
    // more than one of per call.
    let calls: StubbedCall[] = []
    const rounds = () => calls.filter((c) => c.method === 'DELETE' && c.url === '/api/trash').length
    const emptying: StubRoute = {
      method: 'DELETE',
      path: '/api/trash',
      get body() {
        return rounds() === 1 ? { purged: 200, remaining: true } : { purged: 3, remaining: false }
      },
    }
    calls = stubFetch([{ path: '/api/trash', body: { items: two, next_cursor: null } }, emptying])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    await userEvent.click(screen.getByRole('button', { name: 'Empty trash' }))

    // The number is the confirmation: "empty the trash" is agreeable in a way
    // that "delete all 2 items forever" is not.
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Delete all 2 items forever?')).toBeTruthy()

    await userEvent.click(within(dialog).getByRole('button', { name: 'Empty trash' }))

    await waitFor(() => {
      const deletes = calls.filter((c) => c.method === 'DELETE' && c.url === '/api/trash')
      // The route works through a page of roots at a time. Stopping at the
      // first answer leaves a trash that reports itself emptied and is not.
      expect(deletes).toHaveLength(2)
    })
    expect(errorToast).not.toHaveBeenCalled()
  })

  it('leaves a row that could not be restored where it is, and names it', async () => {
    const listing: StubRoute = { path: '/api/trash', body: { items: two, next_cursor: null } }
    stubFetch([
      listing,
      {
        method: 'POST',
        path: '/api/trash/restore',
        body: {
          results: [
            { id: 't1', status: 'ok' },
            { id: 't2', status: 'name_conflict' },
          ],
          remaining: false,
        },
      },
    ])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    await selectAll()
    // The row that could not go back is still in the trash, so the refetch
    // after the call still lists it.
    listing.body = { items: [two[1]], next_cursor: null }
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    await waitFor(() => expect(screen.queryByText('old.txt')).toBeNull())
    expect(screen.getByText('older.txt')).toBeTruthy()
    // Named, because "1 of 2 restored" leaves the person to work out which one
    // is still here by reading the list.
    expect(errorToast).toHaveBeenCalledWith(expect.stringContaining('older.txt'))
  })

  it('dates the rows by when they were thrown away, not when they last changed', async () => {
    stubFetch([
      {
        path: '/api/trash',
        body: {
          items: [node({ id: 't1', name: 'old.txt', updated_at: '2019-03-04T00:00:00Z', deleted_at: '2021-11-09T00:00:00Z' })],
          next_cursor: null,
        },
      },
    ])
    renderApp(<TrashPage />)

    await screen.findByText('old.txt')
    // Both formatted the same way, so what this compares is which field was
    // read — the column header says Trashed, and a row that quietly showed
    // `updated_at` under it would be a lie no one would catch by looking.
    expect(screen.getByText('Trashed')).toBeTruthy()
    expect(screen.getByText(formatWhen('2021-11-09T00:00:00Z'))).toBeTruthy()
    expect(screen.queryByText(formatWhen('2019-03-04T00:00:00Z'))).toBeNull()
  })
})
