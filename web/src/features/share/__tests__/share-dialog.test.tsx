// @vitest-environment jsdom

/**
 * The owner's side of a share link: the row menu and band entry that open the
 * dialog, its four states, and what each button actually sends.
 *
 * Two assertions here are about what must *not* happen. Copy says "copied"
 * only once the clipboard promise has resolved — the server stores a hash and
 * can never show the URL again, so a false "copied" loses the link for good.
 * And New link and Stop sharing send nothing until the question is answered,
 * because neither can be undone.
 */

import { notifyManager } from '@tanstack/react-query'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast, Toaster } from 'sonner'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Outlet, Route, Routes, useParams } from 'react-router'

import type { DriveNode, Me, Share } from '../../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../../test/render'
import { AccountMenu } from '../../../app/AccountMenu'
import { CurrentFolderProvider } from '../../../app/CurrentFolder'
import { SessionsSection } from '../../account/SessionsSection'
import { RequireAuth } from '../../auth/RequireAuth'
import { ResetPage } from '../../auth/ResetPage'
import { meKey } from '../../auth/session'
import { FolderPage } from '../../browser/FolderPage'
import { sharesKey } from '../queries'
import { LINK_NOT_KEPT } from '../ShareDialog'
import { shareUrls } from '../shareUrls'

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
const notes = node({ id: 'f2', kind: 'file', name: 'notes.txt', size: 2048, mime: 'text/plain' })

const DAY = 24 * 60 * 60 * 1000
const URL_1 = 'https://drive.example/s/0123456789abcdef0123456789abcdef0123456789a'
const URL_2 = 'https://drive.example/s/fedcba9876543210fedcba9876543210fedcba98765'

const share = (over: Partial<Share> = {}): Share => ({
  id: 's1',
  node: { id: 'f2', parent_id: 'root-1', name: 'notes.txt', size: 2048, mime: 'text/plain' },
  node_live: true,
  has_password: true,
  // A minute past the week, so rounding reads it as the week it is.
  expires_at: new Date(Date.now() + 7 * DAY + 60_000).toISOString(),
  max_downloads: 2,
  download_count: 1,
  created_at: '2026-08-20T00:00:00Z',
  ...over,
})

const listing: StubRoute[] = [
  { path: '/api/nodes/root-1', body: root },
  { path: '/api/nodes/root-1/children', body: { items: [node({ id: 'f1' }), notes], next_cursor: null } },
]

const FOR_NODE = '/api/shares?node_id=f2'
const none: StubRoute = { path: FOR_NODE, body: { items: [], next_cursor: null } }

function FolderContext() {
  const { id } = useParams()
  return (
    <CurrentFolderProvider folderId={id ?? user.root_id}>
      <Outlet />
    </CurrentFolderProvider>
  )
}

function renderFolder(routes: StubRoute[]) {
  const calls = stubFetch([...listing, ...routes])
  const rendered = renderApp(
    <>
      <Routes>
        <Route element={<FolderContext />}>
          <Route path="/" element={<FolderPage />} />
        </Route>
      </Routes>
      <Toaster />
    </>,
    {
      seed: (client) => {
        client.setQueryData(meKey, user)
        // `/shared`'s list, as it would sit in the cache after a visit there.
        // Every mutation below has to make it stale.
        client.setQueryData(sharesKey, { pages: [{ items: [], next_cursor: null }], pageParams: [undefined] })
      },
    },
  )
  return { calls, ...rendered }
}

/** Opens the dialog from the file's row menu. */
async function openDialog() {
  await screen.findByText('notes.txt')
  await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
  await userEvent.click(await screen.findByRole('menuitem', { name: 'Share' }))
  return screen.findByRole('dialog', { name: 'Share "notes.txt"' })
}

const noClipboard = () => Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  noClipboard()
  shareUrls.clear()
  // Sonner's store is module-scoped: a toast from one case would otherwise be
  // drawn again by the next case's Toaster, and "no toast" would be false.
  toast.dismiss()
})

describe('where Share is offered', () => {
  it('is on a file row’s menu and in the band for one file, and never for a folder', async () => {
    renderFolder([none])
    await screen.findByText('notes.txt')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for notes.txt' }))
    expect(await screen.findByRole('menuitem', { name: 'Share' })).toBeTruthy()
    await userEvent.keyboard('{Escape}')

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Reports' }))
    await screen.findByRole('menuitem', { name: 'Rename' })
    expect(screen.queryByRole('menuitem', { name: 'Share' })).toBeNull()
    await userEvent.keyboard('{Escape}')

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select notes.txt' }))
    const band = screen.getByRole('toolbar', { name: 'Selection actions' })
    expect(within(band).getByRole('button', { name: 'Share' })).toBeTruthy()

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Reports' }))
    expect(within(screen.getByRole('toolbar', { name: 'Selection actions' })).queryByRole('button', { name: 'Share' })).toBeNull()
  })
})

describe('making a link', () => {
  it('posts the four keys with the 7-day default, then shows the URL once with Copy', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-08-23T12:00:00Z'))
    const made: StubRoute = { method: 'POST', path: '/api/shares', status: 201, body: { share: share(), url: URL_1 } }
    const forNode: StubRoute = { ...none }
    const { calls, client } = renderFolder([forNode, made])

    const dialog = await openDialog()
    expect((within(dialog).getByLabelText('Expires') as HTMLSelectElement).value).toBe('7d')
    // The server now has it: what the dialog re-reads after the 201.
    forNode.body = { items: [share()], next_cursor: null }
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create link' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1))
    const post = calls.find((c) => c.method === 'POST')!
    expect(post.url).toBe('/api/shares')
    // Exactly these four — the optional ones as null, never absent.
    expect(post.body).toEqual({
      node_id: 'f2',
      expires_at: '2026-08-30T12:00:00.000Z',
      password: null,
      max_downloads: null,
    })
    expect(Object.keys(post.body as object).sort()).toEqual(['expires_at', 'max_downloads', 'node_id', 'password'])

    const link = (await within(dialog).findByLabelText('Link')) as HTMLInputElement
    expect(link.value).toBe(URL_1)
    expect(link.readOnly).toBe(true)
    expect(within(dialog).getByRole('button', { name: 'Copy link' })).toBeTruthy()
    expect(
      within(dialog).getByText(
        'Drive keeps this link only in this browser — copy it somewhere safe. You can make a new link any time.',
      ),
    ).toBeTruthy()
    expect(within(dialog).queryByText(LINK_NOT_KEPT)).toBeNull()
    // The list on `/shared` is stale now, whichever key it was cached under.
    expect(client.getQueryState(sharesKey)?.isInvalidated).toBe(true)
  })

  it('carries a password, a limit and a chosen preset, and judges the password first', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-08-23T12:00:00Z'))
    const { calls } = renderFolder([
      none,
      { method: 'POST', path: '/api/shares', status: 201, body: { share: share(), url: URL_1 } },
    ])

    const dialog = await openDialog()
    await userEvent.selectOptions(within(dialog).getByLabelText('Expires'), '1d')
    await userEvent.type(within(dialog).getByLabelText('Password'), 'short')
    await userEvent.type(within(dialog).getByLabelText('Download limit'), '2')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create link' }))

    // Seven characters is under the rule the sign-up form shows, and the
    // server would refuse it — so nothing is sent and the field says why.
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
    expect(within(dialog).getByLabelText('Password').getAttribute('aria-invalid')).toBe('true')

    await userEvent.type(within(dialog).getByLabelText('Password'), 'er than eight')
    await userEvent.click(within(dialog).getByLabelText('Show password'))
    expect(within(dialog).getByLabelText('Password').getAttribute('type')).toBe('text')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create link' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1))
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      node_id: 'f2',
      expires_at: '2026-08-24T12:00:00.000Z',
      password: 'shorter than eight',
      max_downloads: 2,
    })
  })

  it('re-reads on a 409 and shows the link that already exists, with no error', async () => {
    const forNode: StubRoute = { ...none }
    const { calls } = renderFolder([
      forNode,
      { method: 'POST', path: '/api/shares', status: 409, body: { code: 'exists', message: 'already shared' } },
    ])

    const dialog = await openDialog()
    // Made in another tab between the dialog opening and the click.
    forNode.body = { items: [share()], next_cursor: null }
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create link' }))

    expect(await within(dialog).findByText(/Password on · 1 of 2 downloads/)).toBeTruthy()
    expect(within(dialog).queryByRole('alert')).toBeNull()
    expect(within(dialog).queryByRole('button', { name: 'Create link' })).toBeNull()
    expect(calls.filter((c) => c.url === FOR_NODE)).toHaveLength(2)
  })
})

describe('a link that exists', () => {
  it('states the facts, and offers New link where Copy would be when this tab never minted it', async () => {
    renderFolder([{ path: FOR_NODE, body: { items: [share({ node_live: false })], next_cursor: null } }])

    const dialog = await openDialog()
    expect(await within(dialog).findByText('Expires in 7 days · Password on · 1 of 2 downloads')).toBeTruthy()
    expect(within(dialog).getByText('In trash — the link is inert until you restore the file')).toBeTruthy()
    // Never a disabled Copy: the URL is not here to copy — and the dialog says so.
    expect(within(dialog).queryByRole('button', { name: 'Copy link' })).toBeNull()
    expect(within(dialog).queryByLabelText('Link')).toBeNull()
    expect(within(dialog).getByText(LINK_NOT_KEPT)).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'New link' })).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'Settings' })).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'Stop sharing' })).toBeTruthy()
  })

  it('shows the read having failed, and re-reads on Try again', async () => {
    const forNode: StubRoute = { path: FOR_NODE, status: 500, body: { code: 'internal', message: 'fell over' } }
    renderFolder([forNode])

    const dialog = await openDialog()
    expect(await within(dialog).findByRole('alert')).toBeTruthy()
    forNode.status = 200
    forNode.body = { items: [], next_cursor: null }
    await userEvent.click(within(dialog).getByRole('button', { name: 'Try again' }))

    expect(await within(dialog).findByRole('button', { name: 'Create link' })).toBeTruthy()
  })

  it('Settings sends expiry and cap always, and keeps an untouched password unsaid', async () => {
    const { calls, client } = renderFolder([
      { path: FOR_NODE, body: { items: [share()], next_cursor: null } },
      { method: 'PATCH', path: '/api/shares/s1', body: {} },
    ])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Settings' }))
    // The password stands apart, on, with its own two actions — no field to
    // re-type and nothing to wipe by accident.
    expect(within(dialog).getByText('Password is on')).toBeTruthy()
    expect(within(dialog).queryByLabelText('Password')).toBeNull()
    await userEvent.selectOptions(within(dialog).getByLabelText('Expires'), 'never')
    await userEvent.clear(within(dialog).getByLabelText('Download limit'))
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    const patch = calls.find((c) => c.method === 'PATCH')!
    // Absent means keep: a password nobody touched never reaches the wire, so
    // an expiry change cannot cost a password nobody can re-type.
    expect(patch.body).toEqual({ expires_at: null, max_downloads: null })
    expect('password' in (patch.body as object)).toBe(false)
    expect(Object.keys(patch.body as object).sort()).toEqual(['expires_at', 'max_downloads'])
    expect(client.getQueryState(sharesKey)?.isInvalidated).toBe(true)
    // Back to the facts once saved.
    expect(await within(dialog).findByRole('button', { name: 'Settings' })).toBeTruthy()
  })

  it('removes or replaces the password only when asked to', async () => {
    const { calls } = renderFolder([
      { path: FOR_NODE, body: { items: [share()], next_cursor: null } },
      { method: 'PATCH', path: '/api/shares/s1', body: {} },
    ])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Settings' }))
    await userEvent.click(within(dialog).getByRole('button', { name: 'Remove password' }))
    expect(within(dialog).getByText('Comes off when you save.')).toBeTruthy()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    const removed = calls.filter((c) => c.method === 'PATCH')[0].body as Record<string, unknown>
    // Null clears — and it is null, not undefined: the key is really there.
    expect(removed.password).toBeNull()
    expect('password' in removed).toBe(true)
    expect(removed.max_downloads).toBe(2)

    // Round two: a new password, typed on purpose.
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Settings' }))
    await userEvent.click(within(dialog).getByRole('button', { name: 'Change password' }))
    await userEvent.type(within(dialog).getByLabelText('Password'), 'a brand new secret')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(2))
    const set = calls.filter((c) => c.method === 'PATCH')[1].body as Record<string, unknown>
    expect(set.password).toBe('a brand new secret')
  })

  it('sends an untouched expiry back byte-for-byte', async () => {
    const at = '2026-08-30T15:45:00.000Z'
    const { calls } = renderFolder([
      { path: FOR_NODE, body: { items: [share({ expires_at: at })], next_cursor: null } },
      { method: 'PATCH', path: '/api/shares/s1', body: {} },
    ])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Settings' }))
    await userEvent.click(within(dialog).getByRole('button', { name: 'Remove password' }))
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    const body = calls.find((c) => c.method === 'PATCH')!.body as Record<string, unknown>
    // The stored instant, not the end of that local day: removing a password
    // must not quietly extend the link by up to a day.
    expect(body.expires_at).toBe(at)
    expect(body.password).toBeNull()
  })
})

describe('Copy', () => {
  it('toasts Link copied only once the clipboard promise has resolved', async () => {
    let land!: () => void
    const writeText = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          land = resolve
        }),
    )
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    shareUrls.set('s1', URL_1)
    renderFolder([{ path: FOR_NODE, body: { items: [share()], next_cursor: null } }])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Copy link' }))

    expect(writeText).toHaveBeenCalledWith(URL_1)
    // The write is still in flight: nothing has been copied yet, and nothing
    // says it has.
    expect(screen.queryByText('Link copied')).toBeNull()

    land()
    expect(await screen.findByText('Link copied')).toBeTruthy()
  })

  it('says Copied on the button for two seconds, then offers Copy link again', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(() => Promise.resolve()) }, configurable: true })
    shareUrls.set('s1', URL_1)
    renderFolder([{ path: FOR_NODE, body: { items: [share()], next_cursor: null } }])

    const dialog = await openDialog()
    const button = await within(dialog).findByRole('button', { name: 'Copy link' })
    // The change is announced: the field is a polite live region.
    expect(button.closest('[aria-live="polite"]')).not.toBeNull()

    // The clock is faked only from here: getting the dialog open waits on real
    // timers, and from the click on, the settling is done by hand — Testing
    // Library's own waiting looks for Jest to decide whether time is faked,
    // and userEvent's own delays would sit on the faked clock, so the click is
    // the plain event.
    vi.useFakeTimers()
    fireEvent.click(button)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(within(dialog).getByRole('button', { name: 'Copied' })).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Copy link' })).toBeNull()
    expect(screen.getByText('Link copied')).toBeTruthy()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_999)
    })
    expect(within(dialog).getByRole('button', { name: 'Copied' })).toBeTruthy()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(within(dialog).getByRole('button', { name: 'Copy link' })).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Copied' })).toBeNull()
  })

  it('drops Copied the moment the link under it is replaced', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(() => Promise.resolve()) }, configurable: true })
    shareUrls.set('s1', URL_1)
    renderFolder([{ path: FOR_NODE, body: { items: [share()], next_cursor: null } }])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Copy link' }))
    expect(await within(dialog).findByRole('button', { name: 'Copied' })).toBeTruthy()

    // A new link lands — what New link's confirm does once the server answers
    // — well inside the two seconds. Nobody has copied this one.
    act(() => shareUrls.set('s1', URL_2))

    expect((within(dialog).getByLabelText('Link') as HTMLInputElement).value).toBe(URL_2)
    expect(within(dialog).getByRole('button', { name: 'Copy link' })).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Copied' })).toBeNull()
  })

  it('selects the URL and swaps the button for a hint where there is no clipboard', async () => {
    noClipboard()
    shareUrls.set('s1', URL_1)
    renderFolder([{ path: FOR_NODE, body: { items: [share()], next_cursor: null } }])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Copy link' }))

    expect(await within(dialog).findByText('Select and copy')).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Copy link' })).toBeNull()
    expect(screen.queryByText('Link copied')).toBeNull()
    const input = within(dialog).getByLabelText('Link') as HTMLInputElement
    expect(document.activeElement).toBe(input)
    expect(input.selectionStart).toBe(0)
    expect(input.selectionEnd).toBe(URL_1.length)
  })
})

describe('the two questions', () => {
  it('New link asks first, sends nothing on Cancel, and regenerates on yes', async () => {
    const { calls, client } = renderFolder([
      { path: FOR_NODE, body: { items: [share()], next_cursor: null } },
      { method: 'PATCH', path: '/api/shares/s1', body: { share: share(), url: URL_2 } },
    ])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'New link' }))

    const ask = await screen.findByRole('dialog', { name: 'Make a new link?' })
    expect(within(ask).getByText('The current link stops working, and the download count starts again at zero.')).toBeTruthy()
    expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(0)

    await userEvent.click(within(ask).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Make a new link?' })).toBeNull())
    expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(0)

    await userEvent.click(within(dialog).getByRole('button', { name: 'New link' }))
    const again = await screen.findByRole('dialog', { name: 'Make a new link?' })
    await userEvent.click(within(again).getByRole('button', { name: 'New link' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1))
    expect(calls.find((c) => c.method === 'PATCH')!.body).toEqual({ action: 'regenerate' })
    // The one time the new URL is shown.
    await waitFor(() => expect((within(dialog).getByLabelText('Link') as HTMLInputElement).value).toBe(URL_2))
    expect(client.getQueryState(sharesKey)?.isInvalidated).toBe(true)
  })

  it('Stop sharing asks first and revokes on yes, leaving the form behind', async () => {
    const forNode: StubRoute = { path: FOR_NODE, body: { items: [share()], next_cursor: null } }
    const { calls, client } = renderFolder([forNode, { method: 'DELETE', path: '/api/shares/s1', status: 204 }])

    const dialog = await openDialog()
    await userEvent.click(await within(dialog).findByRole('button', { name: 'Stop sharing' }))

    const ask = await screen.findByRole('dialog', { name: 'Stop sharing?' })
    expect(within(ask).getByText('Anyone with the link loses access; downloads already started finish.')).toBeTruthy()
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)

    forNode.body = { items: [], next_cursor: null }
    await userEvent.click(within(ask).getByRole('button', { name: 'Stop sharing' }))

    await waitFor(() => expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1))
    expect(calls.find((c) => c.method === 'DELETE')!.url).toBe('/api/shares/s1')
    expect(await screen.findByText('Sharing stopped')).toBeTruthy()
    expect(await within(dialog).findByRole('button', { name: 'Create link' })).toBeTruthy()
    expect(client.getQueryState(sharesKey)?.isInvalidated).toBe(true)
  })
})

describe('the URLs this tab holds', () => {
  beforeEach(() => {
    // The same pinning the shell test uses: the order in which the chrome sees
    // the emptied cache before the navigation is the one worth testing.
    notifyManager.setScheduler((cb) => cb())
  })

  it('are emptied when signing out empties the cache', async () => {
    shareUrls.set('s1', URL_1)
    stubFetch([{ method: 'POST', path: '/api/auth/logout', body: { status: 'ok' } }])
    renderApp(
      <Routes>
        <Route element={<RequireAuth />}>
          <Route path="/" element={<AccountMenu />} />
        </Route>
        <Route path="/login" element={<p>login</p>} />
      </Routes>,
      { seed: (client) => client.setQueryData(meKey, user) },
    )

    await userEvent.click(screen.getByRole('button', { name: 'Your account' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Sign out' }))

    await screen.findByText('login')
    // A share URL is a credential to a file. The next account in this tab must
    // not be handed one to the last account's.
    expect(shareUrls.get('s1')).toBeUndefined()
  })

  it('are emptied by Sign out everywhere, the account page’s own sign-out', async () => {
    shareUrls.set('s1', URL_1)
    stubFetch([
      { path: '/api/auth/sessions', body: { items: [], next_cursor: null } },
      { method: 'POST', path: '/api/auth/logout-all', status: 204 },
    ])
    renderApp(
      <Routes>
        <Route element={<RequireAuth />}>
          <Route path="/" element={<SessionsSection />} />
        </Route>
        <Route path="/login" element={<p>login</p>} />
      </Routes>,
      { seed: (client) => client.setQueryData(meKey, user) },
    )

    await userEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }))
    const ask = await screen.findByRole('dialog', { name: 'Sign out everywhere?' })
    await userEvent.click(within(ask).getByRole('button', { name: 'Sign out everywhere' }))

    await screen.findByText('login')
    expect(shareUrls.get('s1')).toBeUndefined()
  })

  it('are emptied, stored copy included, when the server says there is no session', async () => {
    // A session that lapsed, or was ended by "Sign out everywhere" on another
    // device: no sign-out was clicked in this browser, so nothing else clears
    // the links it kept — and the next load is told 401 by /auth/me.
    shareUrls.set('s1', URL_1)
    expect(localStorage.getItem('drive.share-urls')).not.toBeNull()
    stubFetch([{ path: '/api/auth/me', status: 401, body: { code: 'unauthorized', message: 'sign in' } }])
    renderApp(
      <Routes>
        <Route element={<RequireAuth />}>
          <Route path="/" element={<p>inside</p>} />
        </Route>
        <Route path="/login" element={<p>login</p>} />
      </Routes>,
    )

    await screen.findByText('login')
    expect(screen.queryByText('inside')).toBeNull()
    expect(shareUrls.get('s1')).toBeUndefined()
    expect(localStorage.getItem('drive.share-urls')).toBeNull()
  })

  it('are emptied by a password reset completed in a signed-in tab', async () => {
    shareUrls.set('s1', URL_1)
    stubFetch([{ method: 'POST', path: '/api/auth/password-reset/confirm', status: 204 }])
    // The route is reachable signed in — the account screen sends people here
    // to set a first password — and the server signs every session out.
    renderApp(<ResetPage />, {
      route: '/reset?token=0123456789abcdef',
      seed: (client) => client.setQueryData(meKey, user),
    })

    await userEvent.type(screen.getByLabelText('New password'), 'a fresh password')
    await userEvent.type(screen.getByLabelText('Confirm new password'), 'a fresh password')
    await userEvent.click(screen.getByRole('button', { name: 'Set password' }))

    expect(await screen.findByText('Password set')).toBeTruthy()
    expect(shareUrls.get('s1')).toBeUndefined()
  })
})
