import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Download } from 'lucide-react'
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { Link, useParams, useSearchParams } from 'react-router'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'

import {
  ApiError,
  getSharePreview,
  openShareSession,
  openShareWithPassword,
  shareDownloadHref,
  type ShareMeta,
} from '../../lib/api'
import { fieldClass } from '../../ui/controls'
import { FileIcon } from '../../ui/FileIcon'
import { formatBytes } from '../../ui/format'
import { DriveMark } from '../../ui/icons'
import { MediaPreview } from '../preview/PreviewDialog'
import { previewKey, usePreview } from '../preview/usePreview'
import { useShareMeta } from './queries'

/**
 * `/s/:token` — what a recipient sees. No rail, no dock, no session of ours:
 * a file's name, size and type, a preview where the server will sign one, and
 * Download.
 *
 * The page is shaped by what it must not do. It reads `/meta` first and mints
 * a guest session only once that says the link is live, passwordless and not
 * spent — a revoked, gated or exhausted link writes nothing on the server. The
 * six states sit in one precedence, unavailable > exhausted > gate > file, with
 * "didn't load" and the skeleton outside it, because a server that is merely
 * busy must never be reported as a dead link: that sends a recipient back to
 * an owner who cannot fix it.
 *
 * `/download` is a plain anchor — a navigation, so the browser's own progress
 * and resume survive — and a refusal comes back as a redirect to this page
 * carrying `?reason=`, which is read once and taken out of the URL.
 */

type Reason = 'session' | 'exhausted' | 'gone'

const UNAVAILABLE = "This link isn't available. It may have expired or been turned off."
const EXHAUSTED = 'This link has reached its download limit.'

function asReason(value: string | null): Reason | null {
  return value === 'session' || value === 'exhausted' || value === 'gone' ? value : null
}

export default function SharePage() {
  const { token = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const client = useQueryClient()

  // Read once at mount, then taken out of the URL so a reload is not shown a
  // refusal that has already been read — the `?error=` idiom the sign-in
  // screen uses for Google.
  const [reason] = useState<Reason | null>(() => asReason(params.get('reason')))
  useEffect(() => {
    if (!params.has('reason')) return
    const next = new URLSearchParams(params)
    next.delete('reason')
    setParams(next, { replace: true })
  }, [params, setParams])

  const meta = useShareMeta(token)
  const m = meta.data

  // A bfcache restore does not remount React, and may come back holding a
  // session that died an hour ago. The answer is re-read, and the rest follows.
  const refetchMeta = meta.refetch
  useEffect(() => {
    const onShow = (event: PageTransitionEvent) => {
      if (event.persisted) void refetchMeta()
    }
    window.addEventListener('pageshow', onShow)
    return () => window.removeEventListener('pageshow', onShow)
  }, [refetchMeta])

  const mint = useMutation({
    mutationFn: () => openShareSession(token),
    // The link changed under the page — a password went up, or it was
    // stopped. The ladder re-reads and answers; the guard below stops a loop.
    onError: (err) => {
      if (err instanceof ApiError && (err.status === 401 || err.status === 404)) void refetchMeta()
    },
  })
  // What the person has done at the gate on this page — nothing yet, until
  // they act — and until then `/meta`'s word on whether this browser already
  // holds a live session is what opens a gated file, so a reload does not
  // ask for the password again. An answer given here beats the read: a 401
  // from `/preview` has to bring the gate back over a `session: true` that
  // has gone stale since it was read.
  const [gateAnswer, setGateAnswer] = useState<boolean | null>(null)
  const gatePassed = gateAnswer ?? m?.session === true

  // One mint per token, and only for a link that is live, open and not spent.
  // The ref is what makes StrictMode's double effect a single request; the
  // route is idempotent per browser anyway.
  const mintedFor = useRef<string | null>(null)
  const mintSession = mint.mutate
  useEffect(() => {
    if (m === undefined || m.requires_password || m.exhausted) return
    if (mintedFor.current === token) return
    mintedFor.current = token
    mintSession()
  }, [m, token, mintSession])

  const open = m !== undefined && !m.exhausted && (m.requires_password ? gatePassed : mint.isSuccess)
  const preview = usePreview(open && m.preview ? token : null, getSharePreview)

  // A 401 from `/preview` is the guest session having run out, never a dead
  // end: once per failure, the session is re-minted and the link re-asked —
  // or, behind a password, the gate comes back. Never twice in a row, so a
  // server that keeps refusing gets the card rather than a loop.
  const remintTried = useRef(false)
  const link = preview.link
  useEffect(() => {
    if (link !== undefined) remintTried.current = false
  }, [link])
  const previewError = preview.error
  const gated = m?.requires_password === true
  useEffect(() => {
    if (!(previewError instanceof ApiError) || previewError.status !== 401 || remintTried.current) return
    remintTried.current = true
    if (gated) {
      setGateAnswer(false)
      return
    }
    mintSession(undefined, { onSuccess: () => void client.invalidateQueries({ queryKey: previewKey(token) }) })
  }, [previewError, gated, mintSession, client, token])

  let body: ReactNode
  if (meta.isPending) {
    body = <LoadingCard />
  } else if (meta.error != null) {
    body =
      meta.error instanceof ApiError && meta.error.status === 404 ? (
        <Notice>{UNAVAILABLE}</Notice>
      ) : (
        <DidntLoad error={meta.error} onRetry={() => void refetchMeta()} />
      )
  } else if (m!.exhausted) {
    body = <Notice>{EXHAUSTED}</Notice>
  } else if (m!.requires_password && !gatePassed) {
    body = (
      <Gate
        token={token}
        onPassed={() => {
          remintTried.current = false
          setGateAnswer(true)
        }}
        onGone={() => void refetchMeta()}
      />
    )
  } else {
    body = <FileCard token={token} meta={m!} open={open} preview={preview} />
  }

  // The redirect's one line. For `session` the page is already re-minting, so
  // the line is all there is to say; for the other two the ladder carries the
  // same words whenever `/meta` agrees, and only a link that has since come
  // back — restored, or with its cap raised — needs the line on its own.
  const ladderSays = meta.isError || m?.exhausted === true
  const notice =
    reason === 'session'
      ? 'Your session timed out — reopening.'
      : reason !== null && meta.isSuccess && !ladderSays
        ? reason === 'exhausted'
          ? EXHAUSTED
          : UNAVAILABLE
        : null

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-xl flex-col gap-6 px-4 py-8 text-ink sm:px-6">
      <Link to="/" className="flex w-fit items-center gap-2 text-[15px] font-semibold tracking-tight text-ink">
        <span className="flex h-7 w-7 items-center justify-center rounded-md bg-ink text-canvas">
          <DriveMark className="h-4 w-4" />
        </span>
        Drive
      </Link>

      {notice !== null && (
        <p role="status" className="rounded-control bg-surface-muted px-3 py-2 text-[13px] text-ink-2">
          {notice}
        </p>
      )}

      {body}
    </main>
  )
}

/* ------------------------------------------------------------ the states */

function LoadingCard() {
  return (
    <div aria-hidden="true" className="flex flex-col gap-3 rounded-card border border-line bg-surface p-5 shadow-card">
      <Skeleton className="h-5 w-2/3" />
      <Skeleton className="h-3.5 w-1/3" />
      <Skeleton className="mt-2 h-9 w-32" />
    </div>
  )
}

/** A fact about the link, not a failure of the page. */
function Notice({ children }: { children: string }) {
  return (
    <p role="status" className="rounded-card border border-line bg-surface px-5 py-6 text-sm text-ink-2 shadow-card">
      {children}
    </p>
  )
}

/**
 * The page could not find out. A 429 gets its own words: a page load is three
 * requests out of sixty, so a shared NAT can reach it, and what is true then
 * is that the server is busy — not that the link is dead.
 */
function DidntLoad({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const busy = error instanceof ApiError && error.status === 429
  return (
    <div role="alert" className="flex flex-col items-start gap-3 rounded-card border border-line bg-surface px-5 py-6 shadow-card">
      <p className="text-sm text-ink-2">
        {busy ? 'Too many requests from your network — try again in a minute.' : "Couldn't load this link."}
      </p>
      <Button variant="outline" size="sm" onClick={onRetry}>
        Try again
      </Button>
    </div>
  )
}

function Gate({ token, onPassed, onGone }: { token: string; onPassed: () => void; onGone: () => void }) {
  const [password, setPassword] = useState('')
  const open = useMutation({
    mutationFn: (value: string) => openShareWithPassword(token, value),
    onSuccess: onPassed,
    onError: (err) => {
      // Cleared either way: a wrong password is not worth keeping on screen,
      // and a locked-out one even less so.
      setPassword('')
      if (err instanceof ApiError && err.status === 404) onGone()
    },
  })

  const message =
    open.error instanceof ApiError
      ? open.error.status === 401
        ? "That password didn't work."
        : open.error.status === 429
          ? 'Too many tries — wait a few minutes.'
          : open.error.message
      : open.error
        ? 'Something went wrong. Check your connection and try again.'
        : null

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    if (password === '') return
    open.mutate(password)
  }

  return (
    <form
      onSubmit={onSubmit}
      className="flex flex-col gap-3 rounded-card border border-line bg-surface p-5 shadow-card"
    >
      <p className="text-sm text-ink-2">This link is protected. Enter its password to open it.</p>
      <label className={fieldClass}>
        Password
        <Input
          type="password"
          autoComplete="off"
          autoFocus
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      </label>
      {message !== null && (
        <p role="alert" className="text-[13px] text-danger">
          {message}
        </p>
      )}
      <div>
        <Button type="submit" disabled={open.isPending || password === ''}>
          {open.isPending ? 'Opening…' : 'Open'}
        </Button>
      </div>
    </form>
  )
}

function FileCard({
  token,
  meta,
  open,
  preview,
}: {
  token: string
  meta: ShareMeta
  /** A session exists: the preview may be asked for. */
  open: boolean
  preview: ReturnType<typeof usePreview>
}) {
  const href = shareDownloadHref(token)
  const details = [meta.size === null ? null : formatBytes(meta.size), meta.mime].filter(Boolean).join(' · ')

  return (
    <article className="flex flex-col gap-5">
      <header className="flex items-start gap-3 rounded-card border border-line bg-surface p-5 shadow-card">
        <FileIcon kind="file" name={meta.name} mime={meta.mime} size={32} className="mt-0.5" />
        <div className="min-w-0 flex-1">
          <h1 className="text-[17px] leading-snug font-semibold tracking-tight break-words">{meta.name}</h1>
          {details !== '' && <p className="mt-0.5 text-[13px] text-ink-3">{details}</p>}
        </div>
        <Button asChild className="shrink-0">
          {/* A navigation, in this tab: the answer is a 302 to the bytes, or a
              302 back here with the reason — and this page is where the reason
              has to land. */}
          <a href={href}>
            <Download />
            Download
          </a>
        </Button>
      </header>

      {meta.preview && open && (
        <div className="flex min-h-40 items-center justify-center overflow-hidden rounded-card border border-line bg-canvas p-3 sm:p-4 [&>img]:max-h-[70vh] [&>video]:max-h-[70vh]">
          {preview.link === undefined ? (
            preview.pending ? (
              <p className="text-sm text-ink-3">Loading the preview…</p>
            ) : (
              // Refused or unreadable: the button above is the whole answer.
              <p className="text-sm text-ink-3">No preview — download it to open it.</p>
            )
          ) : preview.broken ? (
            <p className="text-sm text-ink-3">No preview — download it to open it.</p>
          ) : (
            <MediaPreview
              url={preview.link.url}
              mime={preview.link.mime}
              name={meta.name}
              downloadHref={href}
              onBroken={preview.onBroken}
            />
          )}
        </div>
      )}
    </article>
  )
}
