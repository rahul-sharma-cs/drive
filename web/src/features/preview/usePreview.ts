/**
 * The two things the viewer needs that are not rendering: which file the URL
 * says is open, and a presigned link to its bytes that is still alive.
 *
 * `?preview=<id>` rides whatever route is already on screen — a folder, a
 * search — so a preview is a real location: linkable, in the back button, and
 * still there after a reload. Opening pushes an entry, so Back closes; stepping
 * between siblings replaces it, so ten arrow presses leave one entry rather
 * than ten.
 */

import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate, useSearchParams } from 'react-router'

import { getPreview, type PreviewLink } from '../../lib/api'

export const PREVIEW_PARAM = 'preview'

export const previewKey = (id: string) => ['preview', id] as const

/**
 * How long before a link expires the viewer asks for a new one. Comfortably
 * more than a slow refetch, comfortably less than the shortest TTL the server
 * signs with.
 */
const REFRESH_MARGIN_MS = 60_000

/**
 * The floor under that. An expiry already in the past — a browser clock running
 * ahead, or a TTL shorter than the margin — would otherwise mean a timer with
 * no delay at all: it fires, the refetch re-arms it, and every round hands the
 * elements a new URL, which for an `<img>` or a `<video>` is the file pulled
 * off the store again. Fifteen seconds is far longer than a round trip, so the
 * worst case is a slow retry rather than a browser downloading a file on a
 * loop.
 */
const MIN_REFRESH_MS = 15_000

/**
 * The ceiling. `setTimeout` holds its delay in a signed 32-bit int and fires
 * *immediately* on anything larger — so an expiry weeks out, which is the one
 * case that needs no timer at all, is the case that would spin one. Nothing
 * this far ahead is worth arming: no viewer stays open for 24 days, and
 * reopening one signs a fresh link anyway.
 */
const MAX_REFRESH_MS = 2_147_000_000

/**
 * Where a file's name points: this route, plus the parameter.
 *
 * Seeded from the parameters already there, never rebuilt from scratch — on a
 * search screen the query is what the list underneath is made of, and opening a
 * preview must not throw it away.
 */
export function previewTarget(id: string, params: URLSearchParams): { search: string } {
  const next = new URLSearchParams(params)
  next.set(PREVIEW_PARAM, id)
  return { search: `?${next}` }
}

function withoutPreview(params: URLSearchParams): string {
  const next = new URLSearchParams(params)
  next.delete(PREVIEW_PARAM)
  const search = next.toString()
  return search === '' ? '' : `?${search}`
}

export interface PreviewParam {
  /** The file the URL says is open, or null. */
  id: string | null
  /** Move to another file, in place: siblings are one history entry, not many. */
  show: (id: string) => void
  close: () => void
}

export function usePreviewParam(): PreviewParam {
  const [params] = useSearchParams()
  const location = useLocation()
  const navigate = useNavigate()
  const id = params.get(PREVIEW_PARAM)

  // Closing should undo the entry that opened the viewer, so opening and
  // closing a preview leaves the back button exactly where it was. That is only
  // true when the entry underneath really is this route without the parameter:
  // someone who followed a link straight into a preview has nothing of ours
  // behind them, and going back would take them out of the app.
  const previous = useRef({ id, pathname: location.pathname })
  const pushedOnto = useRef<string | null>(null)
  useEffect(() => {
    const was = previous.current
    previous.current = { id, pathname: location.pathname }
    if (id === null) {
      pushedOnto.current = null
      return
    }
    if (was.id === null && was.pathname === location.pathname) pushedOnto.current = location.pathname
  }, [id, location.pathname])

  const show = useCallback(
    (next: string) => void navigate(previewTarget(next, params), { replace: true }),
    [navigate, params],
  )

  const close = useCallback(() => {
    if (pushedOnto.current === location.pathname) {
      pushedOnto.current = null
      void navigate(-1)
      return
    }
    void navigate({ search: withoutPreview(params) }, { replace: true })
  }, [navigate, params, location.pathname])

  return { id, show, close }
}

export interface Preview {
  link: PreviewLink | undefined
  /** The link could not be had — a 415 for an unsupported type, or worse. */
  error: unknown
  pending: boolean
  /** The bytes themselves refused to load, and one fresh link has been tried. */
  broken: boolean
  /** An `<img>`/`<video>`/`<audio>` reported an error. */
  onBroken: () => void
}

/**
 * A live presigned link for `id`.
 *
 * Expiry is a timer, not a `staleTime`: this app turns off refetch-on-focus, so
 * nothing would ever go and get a new link on its own, and an `<iframe>` whose
 * URL has expired fires no error at all — it just shows the store's 403. So the
 * link is replaced a minute before it dies, while the viewer is still open, and
 * every element reading `link.url` re-points at the new one.
 *
 * `id` is an opaque cache key handed to `fetch`: a node id against the owner's
 * preview route by default, or a share token against the public one. The two
 * cannot collide inside `['preview', x]`.
 */
export function usePreview(id: string | null, fetch: (id: string) => Promise<PreviewLink> = getPreview): Preview {
  const client = useQueryClient()

  const query = useQuery({
    queryKey: previewKey(id ?? ''),
    queryFn: () => fetch(id!),
    enabled: id !== null,
    // A link outlives nothing: dropping it on close means the next open asks
    // for a fresh one rather than handing an element a URL that died in the
    // meantime.
    gcTime: 0,
  })

  const expiresAt = query.data?.expires_at
  const updatedAt = query.dataUpdatedAt
  useEffect(() => {
    if (id === null || expiresAt === undefined) return
    const at = new Date(expiresAt).getTime()
    // An unparseable date must not become a zero-delay timer that refetches
    // forever.
    if (!Number.isFinite(at)) return
    const delay = at - Date.now() - REFRESH_MARGIN_MS
    if (delay > MAX_REFRESH_MS) return
    const timer = setTimeout(
      () => void client.invalidateQueries({ queryKey: previewKey(id) }),
      Math.max(delay, MIN_REFRESH_MS),
    )
    return () => clearTimeout(timer)
    // `updatedAt` re-arms the timer after a refetch that answered with the same
    // expiry.
  }, [id, expiresAt, updatedAt, client])

  const [broken, setBroken] = useState(false)
  const retried = useRef(false)
  useEffect(() => {
    setBroken(false)
    retried.current = false
  }, [id])

  const onBroken = useCallback(() => {
    // Once. The likely cause is a link that expired early, and a fresh one
    // fixes that; a second failure is the file, and saying so beats a retry
    // loop against the store.
    if (retried.current) {
      setBroken(true)
      return
    }
    retried.current = true
    void client.invalidateQueries({ queryKey: previewKey(id ?? '') })
  }, [client, id])

  return { link: query.data, error: query.error, pending: query.isPending, broken, onBroken }
}
