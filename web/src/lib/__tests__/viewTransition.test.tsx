// @vitest-environment jsdom

import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { BrowserRouter, Route, Routes, useNavigate } from 'react-router'

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

/**
 * Two folders and a control that opens the second one the way a row does.
 *
 * A real router rather than a `vi.fn()` standing in for one: what is under test
 * is not whether the callback calls something, it is whether the *screen* has
 * changed by the time the callback returns. Only a router that actually
 * re-renders can answer that, and it is the answer a mocked navigate cannot
 * give — it reports a call and leaves the DOM exactly as it was.
 */
function Folders() {
  const navigate = useNavigate()
  return (
    <>
      <button onClick={() => navigateWithTransition(() => void navigate('/folders/b'))}>Open B</button>
      <Routes>
        <Route path="/" element={<p>folder A</p>} />
        <Route path="/folders/b" element={<p>folder B</p>} />
      </Routes>
    </>
  )
}

/**
 * Renders the two folders under a router configured the way the app configures
 * one, and reports what the page said at the instant the transition callback
 * returned — which is the frame the browser snapshots as "after".
 */
function openBInsideATransition(useTransitions: boolean | undefined) {
  reducedMotion(false)
  let snapshot = ''
  const start = vi.fn((callback: () => void) => {
    callback()
    snapshot = document.body.textContent ?? ''
    return { ready: Promise.resolve(), finished: Promise.resolve() }
  })
  give(start)

  window.history.pushState({}, '', '/')
  render(
    <BrowserRouter useTransitions={useTransitions}>
      <Folders />
    </BrowserRouter>,
  )
  expect(document.body.textContent).toContain('folder A')

  fireEvent.click(screen.getByRole('button', { name: 'Open B' }))
  return { start, snapshot }
}

describe('navigateWithTransition', () => {
  it('has the new folder on the page before the transition callback returns', () => {
    const { start, snapshot } = openBInsideATransition(false)

    expect(start).toHaveBeenCalledTimes(1)
    // The whole point. A navigation that lands after `startViewTransition`
    // returns is snapshotted as a page that never changed: the browser
    // crossfades one state against itself and the new folder appears late,
    // with no animation at all.
    expect(snapshot).toContain('folder B')
    expect(snapshot).not.toContain('folder A')
    expect(document.body.textContent).toContain('folder B')
  })

  it('would snapshot the old folder if the router wrapped the navigation in a React transition', () => {
    // The app passes `useTransitions={false}` for exactly this reason, and this
    // is what the default costs: `flushSync` does not flush a transition
    // update, so the callback returns with the old rows still rendered. Left
    // undefined here on purpose — it is the router's own default, and the
    // failure it produces is silent everywhere else.
    const { start, snapshot } = openBInsideATransition(undefined)

    expect(start).toHaveBeenCalledTimes(1)
    expect(snapshot).toContain('folder A')
    expect(snapshot).not.toContain('folder B')
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
