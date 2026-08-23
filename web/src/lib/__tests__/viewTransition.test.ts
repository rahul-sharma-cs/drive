// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from 'vitest'

import { isPlainClick, navigateWithTransition } from '../viewTransition'

/**
 * The helper exists because `<Link viewTransition>` is a no-op under this
 * app's `<BrowserRouter>` — a silent one, which is why the three ways it can
 * end up not animating are asserted here rather than assumed. In every one of
 * them the navigation itself still has to happen: an unsupported browser, or a
 * person who has asked for less movement, gets the folder they clicked on.
 */

/**
 * The DOM lib types `startViewTransition` as always present, so a test that
 * takes it away has to go around the type rather than through it. `as unknown`
 * on the way in for the same reason: the stubs answer the two promises this
 * helper reads and nothing else.
 */
const give = (impl: unknown) => Object.assign(document, { startViewTransition: impl })
const takeAway = () => Reflect.deleteProperty(document, 'startViewTransition')

/** `process` is real under vitest; @types/node is deliberately not installed. */
const proc = (globalThis as unknown as {
  process: { on: (event: string, fn: () => void) => void; off: (event: string, fn: () => void) => void }
}).process

const reducedMotion = (reduce: boolean) =>
  vi.stubGlobal('matchMedia', (media: string) => ({
    media,
    matches: reduce && media.includes('prefers-reduced-motion'),
    addEventListener: () => {},
    removeEventListener: () => {},
  }))

afterEach(() => {
  takeAway()
  vi.unstubAllGlobals()
})

describe('navigateWithTransition', () => {
  it('runs the navigation inside a view transition where the browser has one', () => {
    reducedMotion(false)
    let inside = false
    const start = vi.fn((callback: () => void) => {
      callback()
      return { ready: Promise.resolve(), finished: Promise.resolve() }
    })
    give(start)

    const navigate = vi.fn(() => {
      inside = start.mock.calls.length === 1
    })
    navigateWithTransition(navigate)

    expect(start).toHaveBeenCalledTimes(1)
    expect(navigate).toHaveBeenCalledTimes(1)
    // Not merely called — called *from inside* the callback, which is the whole
    // point: a navigation that lands after `startViewTransition` returns is
    // snapshotted as a page that never changed.
    expect(inside).toBe(true)
  })

  it('navigates plainly when the browser has no view transitions', () => {
    reducedMotion(false)
    const navigate = vi.fn()
    navigateWithTransition(navigate)
    expect(navigate).toHaveBeenCalledTimes(1)
  })

  it('navigates plainly under reduced motion, without starting a transition', () => {
    reducedMotion(true)
    const start = vi.fn()
    give(start)

    const navigate = vi.fn()
    navigateWithTransition(navigate)

    expect(start).not.toHaveBeenCalled()
    expect(navigate).toHaveBeenCalledTimes(1)
  })

  it('swallows the rejection an interrupted transition produces', async () => {
    reducedMotion(false)
    give((callback: () => void) => {
      callback()
      return {
        ready: Promise.reject(new DOMException('skipped', 'AbortError')),
        finished: Promise.reject(new DOMException('skipped', 'AbortError')),
      }
    })

    const unhandled = vi.fn()
    proc.on('unhandledRejection', unhandled)
    navigateWithTransition(() => {})
    // Two turns: one for the rejections to settle, one for Node to decide they
    // were never claimed.
    await new Promise((resolve) => setTimeout(resolve, 0))
    proc.off('unhandledRejection', unhandled)

    expect(unhandled).not.toHaveBeenCalled()
  })
})

describe('isPlainClick', () => {
  const click = (over: Partial<Parameters<typeof isPlainClick>[0]> = {}) => ({
    defaultPrevented: false,
    button: 0,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    ...over,
  })

  it('accepts an unmodified left click', () => {
    expect(isPlainClick(click())).toBe(true)
  })

  it('leaves the browser the clicks that open tabs and windows', () => {
    // Each of these is a way to open the link somewhere else. Calling
    // preventDefault on any of them would take that away.
    expect(isPlainClick(click({ button: 1 }))).toBe(false)
    expect(isPlainClick(click({ metaKey: true }))).toBe(false)
    expect(isPlainClick(click({ ctrlKey: true }))).toBe(false)
    expect(isPlainClick(click({ shiftKey: true }))).toBe(false)
    expect(isPlainClick(click({ altKey: true }))).toBe(false)
    expect(isPlainClick(click({ defaultPrevented: true }))).toBe(false)
  })
})
