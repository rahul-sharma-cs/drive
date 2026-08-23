import { LoaderCircle } from 'lucide-react'
import { lazy, Suspense, useEffect, useState } from 'react'

import type { PreviewDialogProps } from './PreviewDialog'
import { usePreviewParam } from './usePreview'

/**
 * The viewer, kept out of the chunk that has to arrive before anything renders.
 *
 * Most visits to a folder never open a file, so the image/video/audio/PDF/text
 * bodies, their expiry timer and their keyboard handling do not belong in the
 * bundle that stands between a person and their file list. This is the boundary
 * that splits them off; everything about the viewer's behaviour lives in
 * `PreviewDialog.tsx` and is unchanged by being on the other side of it.
 *
 * Two details make the split invisible rather than merely cheap:
 *
 *  - **It mounts on the first open and then stays mounted.** The dialog animates
 *    itself out, which it can only do while it is still on the page — unmounting
 *    it the moment `?preview=` disappears would replace the close animation with
 *    the picture vanishing.
 *  - **The chunk is warmed when the browser is idle.** Lazy-loading buys a
 *    smaller first paint; it must not buy it by making the first click on a file
 *    name wait for a network round trip. By the time anyone has read a row and
 *    aimed at it, the chunk is already there.
 *
 * The warming is a strong bet, not a guarantee — a click inside the first
 * second, a cold cache, a bad connection — so the fallback below is what the
 * click gets in the meantime, and it is not nothing. Nothing is the one answer
 * a click must never get: the page sits there unchanged, and the honest reading
 * of that is "it did not register", which is what makes a person click again.
 */
const Viewer = lazy(() => import('./PreviewDialog'))

/** Kicks the chunk off without competing with anything on the critical path. */
function warm(): () => void {
  const idle = window.requestIdleCallback
  if (typeof idle !== 'function') {
    const timer = setTimeout(() => void import('./PreviewDialog'), 1200)
    return () => clearTimeout(timer)
  }
  const handle = idle(() => void import('./PreviewDialog'), { timeout: 3000 })
  return () => window.cancelIdleCallback?.(handle)
}

export function PreviewDialog(props: PreviewDialogProps) {
  const { id } = usePreviewParam()
  const [live, setLive] = useState(id !== null)

  useEffect(() => {
    if (id !== null) setLive(true)
  }, [id])

  useEffect(() => {
    if (live) return
    return warm()
  }, [live])

  if (!live) return null
  return (
    <Suspense fallback={<Opening />}>
      <Viewer {...props} />
    </Suspense>
  )
}

/**
 * The viewer's own scrim, drawn before the viewer exists.
 *
 * It is the same `.scrim` the real dialog dims the page with, so what a person
 * sees is the viewer opening — the page goes back, the panel arrives a moment
 * later — rather than two separate events. On a warm chunk it is never painted
 * at all.
 *
 * `aria-hidden`, and deliberately: the dialog that lands a frame later
 * announces itself, and a status message for something that is usually gone
 * inside 50 ms is noise. Nobody can interact with it either — there is nothing
 * here to interact with yet, and the click that opened it is already spent.
 */
function Opening() {
  return (
    <div aria-hidden="true" className="scrim fixed inset-0 z-50 grid place-items-center">
      <LoaderCircle className="size-5 animate-spin text-ink-2" />
    </div>
  )
}
