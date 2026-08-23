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
    <Suspense fallback={null}>
      <Viewer {...props} />
    </Suspense>
  )
}
