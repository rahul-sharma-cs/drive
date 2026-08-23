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

const three = [...two, node({ id: 't3', name: 'oldest.txt' })]

const ok = (id: string) => ({ id, status: 'ok' })
const pending = (id: string) => ({ id, status: 'pending' })

/**
 * A `/api/trash/restore` that answers differently on each round.
 *
 * The body is read at call time, so a getter is what lets the second call see
 * something different: the loop runs inside the mutation and a test cannot step
 * into the middle of it. It counts requests rather than its own reads, which
 * the harness makes more than one of per call.
 */
function restoreRounds(answers: Record<number, unknown>, seen: () => number): StubRoute {
  return {
    method: 'POST',
    path: '/api/trash/restore',
    get body() {
      // A round past the last one scripted answers everything at once, so a
      // client that would not stop on its own finishes loudly wrong instead of
      // spinning the whole suite to a halt.
      return answers[seen()] ?? { results: three.map((n) => ok(n.id)), remaining: false }
    },
  }
}

const selectAll = () => userEvent.click(screen.getByRole('checkbox', { name: /^Select all/ }))

afterEach(() => {
  vi.unstubAllGlobals()
  errorToast.mockClear()
})

describe('the trash in bulk', () => {
  it('pages, and a select-all covers both pages rather than the first one', async () => {
    const calls = stubFetch([
      // Exact string, so the second page's `?cursor=` cannot match this route.
      { path: '/api/trash', body: { items: two, next_cursor: 'c1' } },
      { path: /^\/api\/trash\?cursor=c1$/, body: { items: [three[2]], next_cursor: null } },
      {
        method: 'POST',
        path: '/api/trash/purge',
        body: { results: three.map((n) => ok(n.id)), remaining: false },
      },
    ])
    renderApp(<TrashPage />)
    await screen.findByText('old.txt')

    // With a page still unfetched the confirmation must not put a number on it.
    await userEvent.click(screen.getByRole('button', { name: 'Empty trash' }))
    expect(screen.getByRole('heading', { name: 'Delete everything in the trash forever?' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))
    await screen.findByText('oldest.txt')
    expect(calls.filter((c) => c.url.startsWith('/api/trash?')).map((c) => c.url)).toEqual([
      '/api/trash?cursor=c1',
    ])

    // Both pages are loaded, so both are what "select all" means and what the
    // confirmation is allowed to count.
    await selectAll()
    await waitFor(() => expect(screen.getByText('3 selected')).toBeTruthy())

    await userEvent.click(screen.getByRole('button', { name: 'Delete forever' }))
    await waitFor(() => expect(calls.some((c) => c.url === '/api/trash/purge')).toBe(true))
    const purge = calls.find((c) => c.url === '/api/trash/purge')!
    expect((purge.body as { ids: string[] }).ids).toEqual(['t1', 't2', 't3'])
  })

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

  it('does not destroy the selection on the Delete key', async () => {
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
    // In a folder this key means "move to the trash", and the trash is where
    // it can be undone from. On this screen the same key would mean "destroy
    // it", with no dialog in front of it and nothing behind it — a keystroke
    // away from a file that is not coming back. So it means nothing here, the
    // way Enter does.
    fireEvent.keyDown(screen.getByTestId('file-list'), { key: 'Delete' })
    fireEvent.keyDown(screen.getByTestId('file-list'), { key: 'Backspace' })

    // Given a moment to happen, so that the assertion is about the request not
    // being sent rather than about it not having been sent yet.
    await waitFor(() => expect(screen.getByText('2 selected')).toBeTruthy())
    expect(calls.filter((c) => c.url === '/api/trash/purge')).toHaveLength(0)

    // And the deliberate way to do it still works, so what is under test is the
    // key and not a screen that lost its purge.
    await userEvent.click(screen.getByRole('button', { name: 'Delete forever' }))
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

  it('asks the second round for the ids the first never reached, and for nothing else', async () => {
    // The route works under a wall clock. It got through one root, ran out of
    // budget, and handed the other two back untouched.
    let calls: StubbedCall[] = []
    const rounds = () => calls.filter((c) => c.url === '/api/trash/restore').length
    const restoring = restoreRounds(
      {
        1: { results: [ok('t1'), pending('t2'), pending('t3')], remaining: true },
        2: { results: [ok('t2'), ok('t3')], remaining: false },
      },
      rounds,
    )
    calls = stubFetch([{ path: '/api/trash', body: { items: three, next_cursor: null } }, restoring])
    renderApp(<TrashPage />)

    await screen.findByText('oldest.txt')
    await selectAll()
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    await waitFor(() => expect(rounds()).toBe(2))
    const posts = calls.filter((c) => c.url === '/api/trash/restore')
    expect(posts[0].body).toEqual({ ids: ['t1', 't2', 't3'] })
    // Exactly the two that came back pending, in the order they were handed
    // over. Sending the whole selection again would restore the first one a
    // second time — by then it is out of the trash, so the answer is
    // `not_found`, which this client reads as "already done" and nobody ever
    // hears that a row came back twice.
    expect(posts[1].body).toEqual({ ids: ['t2', 't3'] })

    // Nothing was left pending after the second round, so there is no third.
    await waitFor(() => expect(screen.queryByText('Restore')).toBeTruthy())
    expect(rounds()).toBe(2)
    expect(errorToast).not.toHaveBeenCalled()
  })

  it('gives up on a round that got through none of them, rather than asking forever', async () => {
    // The server gets to at least one root per call, so a round that resolved
    // nothing resolves nothing on the next pass either. `remaining: true` is
    // still what it says — a client that trusted it would spin against a server
    // in this state for as long as the tab is open.
    let calls: StubbedCall[] = []
    const rounds = () => calls.filter((c) => c.url === '/api/trash/restore').length
    const restoring = restoreRounds(
      {
        1: { results: [ok('t1'), pending('t2'), pending('t3')], remaining: true },
        2: { results: [pending('t2'), pending('t3')], remaining: true },
      },
      rounds,
    )
    calls = stubFetch([{ path: '/api/trash', body: { items: three, next_cursor: null } }, restoring])
    renderApp(<TrashPage />)

    await screen.findByText('oldest.txt')
    await selectAll()
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))

    // Said out loud, because the rows are still in the list and a screen that
    // simply stopped would look like one that had finished.
    await waitFor(() =>
      expect(errorToast).toHaveBeenCalledWith('Some of the trash is still there. Try again.'),
    )
    expect(rounds()).toBe(2)
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
