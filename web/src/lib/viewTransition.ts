import { flushSync } from 'react-dom'

/**
 * Navigating between folders, as one movement rather than a swap.
 *
 * Opening a folder replaces every row on the screen at once; without a
 * transition the list simply is one thing and then another, which is the
 * moment a file manager feels like a page reload instead of a place you are
 * moving around in. A view transition crossfades the two states for free — the
 * browser snapshots what is there, we change the DOM, it animates between the
 * two — and it costs one call rather than a pair of enter/exit animations that
 * have to be kept in step.
 *
 * Not `<Link viewTransition>`: that prop is React Router's, and under this
 * app's declarative `<BrowserRouter>` it is a silent no-op. Nothing about it
 * fails loudly, which is exactly why it is worth saying so here.
 *
 * `flushSync` is what makes it work at all. `startViewTransition` snapshots the
 * page, runs the callback, and snapshots again — so the DOM has to have
 * actually changed by the time the callback returns, and React's default
 * batching would leave the update sitting in a queue until after.
 *
 * `Document.startViewTransition` is typed as always present and is not — no
 * Safari before 18 and no Firefox before 144 has it — so the runtime check
 * below is the real gate, whatever the DOM lib says.
 */

/** Reduced motion asks for no crossfade at all here: it is pure movement. */
function wantsMotion(): boolean {
  return !window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
}

export function navigateWithTransition(navigate: () => void): void {
  if (typeof document.startViewTransition !== 'function' || !wantsMotion()) {
    navigate()
    return
  }
  const transition = document.startViewTransition(() => flushSync(navigate))
  // A transition interrupted by the next one rejects both of these. That is an
  // ordinary outcome — somebody clicked twice — and must not surface as an
  // unhandled rejection.
  transition.ready.catch(() => {})
  transition.finished.catch(() => {})
}

/**
 * True when a click on a link is the plain kind that should be handled in the
 * page. A middle click, a modified click and an already-handled click all
 * belong to the browser: they open tabs and windows, and taking them over
 * would break the one thing a real `<a href>` is for.
 */
export function isPlainClick(event: {
  defaultPrevented: boolean
  button: number
  metaKey: boolean
  ctrlKey: boolean
  shiftKey: boolean
  altKey: boolean
}): boolean {
  return (
    !event.defaultPrevented &&
    event.button === 0 &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.shiftKey &&
    !event.altKey
  )
}
