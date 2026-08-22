// @vitest-environment jsdom

import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { Route, Routes, useNavigate } from 'react-router'

import type { DriveNode, Me } from '../../lib/api'
import { renderApp, stubFetch, type StubRoute } from '../../test/render'
import { meKey } from '../../features/auth/session'
import { FolderPage } from '../../features/browser/FolderPage'
import { AppLayout } from '../AppLayout'

// The real actions drive the singleton engine, which would spawn Web Workers
// and open IndexedDB. What this file is about is the shell: which command
// reaches the engine, bound to which folder.
const enqueue = vi.fn()
vi.mock('../../features/upload/ui/engineStore', () => ({
  uploadActions: {
    enqueue: (...args: unknown[]) => enqueue(...args),
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
  display_name: 'Ada Lovelace',
  root_id: 'root-1',
  email_verified_at: '2026-08-17T00:00:00Z',
}

const root: DriveNode = {
  id: 'root-1',
  parent_id: null,
  kind: 'folder',
  name: 'root',
  size: null,
  mime: null,
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
}

/** What the chrome itself always asks for, whatever screen is under it. */
const chrome: StubRoute[] = [
  { path: '/api/uploads', body: { items: [], next_cursor: null } },
  { path: '/api/usage', body: { used: 0, quota: 1_000_000 } },
]

/**
 * The layout over placeholder screens. The pages are deliberately inert: this
 * file is about the shell, and a real screen would bring a second `nav` and a
 * second set of buttons into every query.
 */
function renderShell(routes: StubRoute[] = [], { route = '/', page }: { route?: string; page?: ReactNode } = {}) {
  const calls = stubFetch([...chrome, ...routes])
  const rendered = renderApp(
    <Routes>
      <Route element={<AppLayout />}>
        <Route index element={page ?? <p>my drive</p>} />
        <Route path="/folders/:id" element={page ?? <p>a folder</p>} />
        <Route path="/trash" element={<p>trash</p>} />
        <Route path="/search" element={<p>search</p>} />
      </Route>
      <Route path="/login" element={<p>login</p>} />
    </Routes>,
    { route, seed: (client) => client.setQueryData(meKey, user) },
  )
  return { calls, ...rendered }
}

/**
 * jsdom ships no `matchMedia` at all, so the layout's breakpoint listener has
 * nothing to attach to unless a test hands it one. Records what the app asked
 * about, and lets the test announce that the window crossed it.
 */
function stubBreakpoint() {
  const queries: string[] = []
  const listeners = new Set<(e: MediaQueryListEvent) => void>()
  vi.stubGlobal('matchMedia', (media: string) => {
    queries.push(media)
    return {
      media,
      matches: false,
      addEventListener: (_: string, fn: (e: MediaQueryListEvent) => void) => listeners.add(fn),
      removeEventListener: (_: string, fn: (e: MediaQueryListEvent) => void) => listeners.delete(fn),
    }
  })
  return {
    queries,
    cross: (matches: boolean) =>
      act(() => {
        for (const fn of listeners) fn({ matches } as MediaQueryListEvent)
      }),
  }
}

beforeEach(() => {
  enqueue.mockClear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the New menu and the upload seam', () => {
  it('opens the files picker from the rail and enqueues into the folder on screen', async () => {
    renderShell([], { route: '/folders/f9' })

    const input = screen.getByLabelText('Upload files')
    const opened = vi.spyOn(input, 'click')

    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Upload files' }))
    // The menu item's whole job is to reach the hidden input the layout mounts.
    expect(opened).toHaveBeenCalledTimes(1)

    const one = new File(['a'], 'one.txt')
    const two = new File(['b'], 'two.txt')
    await userEvent.upload(input, [one, two])

    expect(enqueue.mock.calls).toEqual([
      [one, 'f9'],
      [two, 'f9'],
    ])
  })

  it('re-reads the folder an upload started in, not the one you walked to while it ran', async () => {
    // The folder walk is held open across a navigation. A picker that read the
    // current folder on its way out would re-read the trash's root instead of
    // the folder the files are actually landing in.
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    const answer = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })
    const fetchMock = vi.fn(async (url: string) => {
      if (url === '/api/folders') {
        await held
        return answer({ id: 'made-1' })
      }
      if (url === '/api/uploads') return answer({ items: [], next_cursor: null })
      if (url === '/api/usage') return answer({ used: 0, quota: 1_000_000 })
      throw new Error(`unstubbed request: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const { client } = renderApp(
      <Routes>
        <Route element={<AppLayout />}>
          <Route path="/folders/:id" element={<p>a folder</p>} />
          <Route path="/trash" element={<p>trash</p>} />
        </Route>
      </Routes>,
      { route: '/folders/f9', seed: (c) => c.setQueryData(meKey, user) },
    )
    const invalidate = vi.spyOn(client, 'invalidateQueries')

    const nested = new File(['x'], 'report.pdf')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'tree/report.pdf' })
    await userEvent.upload(screen.getByLabelText('Upload folder'), [nested])
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/folders', expect.anything()))

    await userEvent.click(screen.getByRole('link', { name: 'Trash' }))
    await screen.findByText('trash')

    release()
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['children', 'f9'] }))
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['children', 'root-1'] })
  })

  it('offers New wherever something can be created, and never in the trash', async () => {
    renderShell([], { route: '/folders/f9' })
    expect(screen.getByRole('button', { name: 'New' })).toBeTruthy()

    await userEvent.click(screen.getByRole('link', { name: 'Trash' }))
    await screen.findByText('trash')
    await waitFor(() => expect(screen.queryByRole('button', { name: 'New' })).toBeNull())
  })

  it('names My Drive as the destination on the search screen, and uploads there', async () => {
    renderShell([], { route: '/search' })

    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    // No "here" to upload into on a results screen, so the item says where the
    // files will actually go.
    expect(await screen.findByRole('menuitem', { name: 'Upload files to My Drive' })).toBeTruthy()
    await userEvent.click(screen.getByRole('menuitem', { name: 'Upload files to My Drive' }))

    const one = new File(['a'], 'one.txt')
    await userEvent.upload(screen.getByLabelText('Upload files'), [one])
    expect(enqueue).toHaveBeenCalledWith(one, 'root-1')
  })

  it('creates a folder in the folder on screen, and re-reads it', async () => {
    // Deliberately NOT the root: the menu has no folder of its own and has to
    // ask where the app is, so a destination that happened to be the root would
    // pass whether it asked or not.
    const { calls } = renderShell(
      [
        { path: '/api/nodes/root-1', body: root },
        { path: '/api/nodes/f9', body: { ...root, id: 'f9', parent_id: 'root-1', name: 'Reports' } },
        { path: '/api/nodes/f9/children', body: { items: [], next_cursor: null } },
        { method: 'POST', path: '/api/folders', body: { ...root, id: 'new-1', name: 'Invoices' } },
      ],
      { route: '/folders/f9', page: <FolderPage /> },
    )

    await screen.findByText('This folder is empty.')
    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'New folder' }))
    await userEvent.type(await screen.findByLabelText('Name'), 'Invoices')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST')
      expect(post?.url).toBe('/api/folders')
      expect(post?.body).toEqual({ parent_id: 'f9', name: 'Invoices' })
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => expect(calls.filter((c) => c.url === '/api/nodes/f9/children').length).toBeGreaterThan(1))
  })

  it('leaves the page interactive after the menu hands off to a dialog (Radix pointer-events)', async () => {
    renderShell([], { route: '/folders/f9' })

    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    // A non-modal menu must not take the page away from the pointer at all —
    // the row underneath it is a drop target the whole time it is open.
    expect(document.body.style.pointerEvents).not.toBe('none')

    await userEvent.click(await screen.findByRole('menuitem', { name: 'New folder' }))
    await screen.findByRole('dialog')
    // The dialog is modal, so it does take it, and has to give it back.
    expect(document.body.style.pointerEvents).toBe('none')

    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => expect(document.body.style.pointerEvents).not.toBe('none'))
  })

  it('gives the page back to the pointer when the dialog is dismissed by its scrim', async () => {
    renderShell([], { route: '/folders/f9' })

    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'New folder' }))
    await screen.findByRole('dialog')
    // Radix arms the outside-pointer listener on a 0 ms timer, and `findBy`
    // resolves before that timer runs.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    // A different Radix path from Escape — `onPointerDownOutside` rather than
    // `onEscapeKeyDown` — and the one a person on a mouse actually takes. Both
    // events, because a modal Dialog defers a left-button dismissal to the click
    // that follows; and fired rather than driven by userEvent, because the scrim
    // inherits the `pointer-events: none` the dialog puts on <body>, which is
    // the very thing under test.
    const scrim = document.querySelector('.scrim')
    expect(scrim).toBeTruthy()
    fireEvent.pointerDown(scrim as Element)
    fireEvent.click(scrim as Element)

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => expect(document.body.style.pointerEvents).not.toBe('none'))
  })
})

describe('the rail and the account menu', () => {
  it('keeps exactly one rail on the page, drawer or not, and closes the drawer on navigate', async () => {
    renderShell([], { route: '/folders/f9' })
    // `hidden: true` counts what is MOUNTED rather than what is reachable: a
    // modal drawer marks everything behind it `aria-hidden`, so a rail left
    // rendered under it would be invisible to the accessible-name query and
    // the assertion would pass with two of them on the page.
    expect(screen.getAllByRole('navigation', { hidden: true })).toHaveLength(1)

    await userEvent.click(screen.getByRole('button', { name: 'Open navigation' }))
    const drawer = await screen.findByRole('dialog')
    expect(screen.getAllByRole('navigation', { hidden: true })).toHaveLength(1)

    await userEvent.click(within(drawer).getByRole('link', { name: 'Trash' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(screen.getAllByRole('navigation', { hidden: true })).toHaveLength(1)
  })

  it('hands focus back to the hamburger when the drawer closes', async () => {
    renderShell([], { route: '/folders/f9' })
    const hamburger = screen.getByRole('button', { name: 'Open navigation' })
    // Both come from being the drawer's registered trigger. A plain button that
    // flips a boolean announces neither — and leaves Radix nothing to hand focus
    // back to on the way out.
    expect(hamburger.getAttribute('aria-haspopup')).toBe('dialog')
    expect(hamburger.getAttribute('aria-expanded')).toBe('false')

    await userEvent.click(hamburger)
    await screen.findByRole('dialog')
    expect(hamburger.getAttribute('aria-expanded')).toBe('true')

    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    // <body> is where an unregistered trigger leaves it: a keyboard user who
    // dismisses the drawer is dropped at the top of the document.
    await waitFor(() => expect(document.activeElement).toBe(hamburger))
    expect(hamburger.getAttribute('aria-expanded')).toBe('false')
  })

  it('closes the drawer on a navigation the rail never sees, like Back', async () => {
    let go!: ReturnType<typeof useNavigate>
    function Screen() {
      go = useNavigate()
      return <p>a folder</p>
    }

    renderShell([], { route: '/folders/f9', page: <Screen /> })
    await act(async () => {
      await go('/trash')
    })
    await screen.findByText('trash')

    await userEvent.click(screen.getByRole('button', { name: 'Open navigation' }))
    await screen.findByRole('dialog')

    // Back is a route change with no click on a rail link behind it, so a close
    // wired to the links alone leaves the drawer open over a screen it no longer
    // describes.
    await act(async () => {
      await go(-1)
    })

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(await screen.findByText('a folder')).toBeTruthy()
    expect(screen.getAllByRole('navigation', { hidden: true })).toHaveLength(1)
  })

  it('closes the drawer when the window grows past the width the hamburger hides at', async () => {
    const viewport = stubBreakpoint()
    renderShell([], { route: '/folders/f9' })
    // The same 48rem Tailwind compiles `md:` from — `md:hidden` takes the
    // hamburger away at exactly this width and `md:flex` brings the fixed rail
    // back, so a listener on any other width would be the wrong one.
    expect(viewport.queries).toContain('(min-width: 48rem)')

    await userEvent.click(screen.getByRole('button', { name: 'Open navigation' }))
    await screen.findByRole('dialog')
    expect(screen.queryByRole('complementary', { hidden: true })).toBeNull()

    viewport.cross(true)

    // Otherwise: a modal panel and its scrim sitting over a desktop layout whose
    // only way to reopen — or close — it is `md:hidden`.
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(screen.getByRole('complementary', { hidden: true })).toBeTruthy()
    expect(screen.getAllByRole('navigation', { hidden: true })).toHaveLength(1)
  })

  it('signs out from the account menu, which is where it lives now', async () => {
    const { calls } = renderShell([{ method: 'POST', path: '/api/auth/logout', body: { status: 'ok' } }])

    // Not a button sitting in the rail beside the destinations any more.
    expect(screen.queryByRole('button', { name: 'Sign out' })).toBeNull()

    const face = screen.getByRole('button', { name: 'Your account' })
    // First and last initial, not an empty circle.
    expect(within(face).getByText('AL')).toBeTruthy()
    await userEvent.click(face)
    const menu = await screen.findByRole('menu')
    expect(within(menu).getByText(user.email)).toBeTruthy()
    await userEvent.click(within(menu).getByRole('menuitem', { name: 'Sign out' }))

    await waitFor(() => {
      const post = calls.find((c) => c.url === '/api/auth/logout')
      expect(post?.method).toBe('POST')
    })
    expect(await screen.findByText('login')).toBeTruthy()
  })
})
