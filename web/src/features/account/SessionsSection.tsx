import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

import { listSessions, logoutAll, revokeSession, type AuthSession, type Page } from '../../lib/api'
import { SkeletonRows } from '../../ui/controls'
import { formatWhen } from '../../ui/when'
import { useSetSession } from '../auth/session'

const sessionsKey = ['auth', 'sessions'] as const

/**
 * Every browser currently holding a session, and the two ways to end one.
 *
 * The session this page is being read in carries no Revoke: an affordance that
 * signs you out of the screen you are standing on is a trap, and "Sign out
 * everywhere" — which does include it — is right there and says so.
 */
export function SessionsSection() {
  const client = useQueryClient()
  const navigate = useNavigate()
  const setSession = useSetSession()
  const [confirming, setConfirming] = useState(false)

  const sessions = useQuery({ queryKey: sessionsKey, queryFn: listSessions })

  const revoke = useMutation({
    mutationFn: revokeSession,
    // The list is dropped to, not refetched from, the server: the row is gone
    // because the DELETE said so, and a second GET would only re-render the
    // same answer a beat later.
    onSuccess: (_result, id) => {
      client.setQueryData(sessionsKey, (was: Page<AuthSession> | undefined) =>
        was ? { ...was, items: was.items.filter((s) => s.id !== id) } : was,
      )
      toast.success('Signed that device out')
    },
    onError: (err: unknown) => toast.error((err as Error)?.message ?? 'Could not sign that device out'),
  })

  const everywhere = useMutation({
    mutationFn: logoutAll,
    /**
     * Order matters, and so does what is *not* here. The cookie is gone
     * server-side, so the cached `me` has to go too — but everything on this
     * screen is under `useSession()`, which throws when that cache is empty.
     * Navigating first means the route change is already queued when the cache
     * empties, so the account screen unmounts in the same commit instead of
     * rendering one frame without a user. Closing the dialog here as well split
     * that into two commits (a modal's focus restore flushes synchronously) and
     * the screen rendered — and threw — in between; the navigation unmounts the
     * dialog anyway.
     */
    onSuccess: () => {
      void navigate('/login', { replace: true })
      setSession(null)
    },
  })

  const items = sessions.data?.items ?? []

  return (
    <section aria-labelledby="sessions-heading" className="flex flex-col gap-1.5">
      <h2 id="sessions-heading" className="text-[15px] font-semibold text-ink">
        Where you’re signed in
      </h2>
      <p className="text-[13px] text-ink-3">
        Each browser that has signed in and not signed out. Revoke anything you don’t recognise.
      </p>

      <div className="mt-4 overflow-hidden rounded-card border border-line bg-surface">
        {sessions.isPending && <SkeletonRows rows={2} />}

        {sessions.error && (
          <div role="alert" className="flex flex-col items-start gap-2 px-4 py-5">
            <p className="text-sm text-ink-2">The session list didn’t load.</p>
            <Button variant="outline" size="sm" onClick={() => void sessions.refetch()}>
              Try again
            </Button>
          </div>
        )}

        {sessions.isSuccess && (
          <ul className="divide-y divide-line">
            {items.map((s) => (
              <li key={s.id} className="flex items-center gap-3 px-4 py-3">
                <div className="min-w-0 flex-1">
                  <p className="flex items-center gap-2 text-[13px] font-medium text-ink">
                    <span className="truncate">{shortAgent(s.user_agent)}</span>
                    {s.current && (
                      <span className="shrink-0 rounded-full bg-teal-soft px-2 py-0.5 text-[11px] font-medium text-teal">
                        This device
                      </span>
                    )}
                  </p>
                  <p className="numeric mt-0.5 truncate text-ink-3">
                    {s.ip ?? 'address unknown'} · last seen{' '}
                    {s.last_seen_at ? formatWhen(s.last_seen_at) : 'not since sign-in'} · signed in{' '}
                    {formatWhen(s.created_at)}
                  </p>
                </div>
                {!s.current && (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={revoke.isPending}
                    onClick={() => revoke.mutate(s.id)}
                    aria-label={`Revoke ${shortAgent(s.user_agent)}`}
                  >
                    Revoke
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="mt-4">
        <Button variant="outline" onClick={() => setConfirming(true)}>
          Sign out everywhere
        </Button>
      </div>

      <Dialog open={confirming} onOpenChange={setConfirming}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Sign out everywhere?</DialogTitle>
            <DialogDescription>
              Every browser signed in to this account is signed out, including this one. Uploads in flight elsewhere
              stop; their finished parts are kept and resume after the next sign-in.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={everywhere.isPending} onClick={() => everywhere.mutate()}>
              {everywhere.isPending ? 'Signing out…' : 'Sign out everywhere'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}

/**
 * A user-agent string as a device, not as a header. The full string is what the
 * server stores; what a person needs in order to recognise a row is the browser
 * and the machine it runs on.
 *
 * Order matters: Edge and Opera both claim Chrome, and Chrome claims Safari.
 */
function shortAgent(ua: string | null): string {
  if (!ua || ua.trim() === '') return 'Unknown device'

  const browser = /\bEdge?[A-Za-z]*\//.test(ua)
    ? 'Edge'
    : /\bOPR\/|\bOpera\//.test(ua)
      ? 'Opera'
      : /\bFirefox\//.test(ua)
        ? 'Firefox'
        : /\bChrome\//.test(ua)
          ? 'Chrome'
          : /\bSafari\//.test(ua)
            ? 'Safari'
            : null

  const os = /\bWindows\b/.test(ua)
    ? 'Windows'
    : /\bAndroid\b/.test(ua)
      ? 'Android'
      : /\biPhone\b|\biPad\b/.test(ua)
        ? 'iOS'
        : /\bMac OS X\b|\bMacintosh\b/.test(ua)
          ? 'macOS'
          : /\bLinux\b|\bX11\b/.test(ua)
            ? 'Linux'
            : null

  if (!browser && !os) return ua.length > 48 ? `${ua.slice(0, 48)}…` : ua
  if (!browser) return os as string
  return os ? `${browser} on ${os}` : browser
}
