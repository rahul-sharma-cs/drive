import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
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

import { ApiError, listIdentities, unlinkIdentity, type Identity, type Page } from '../../lib/api'
import { FormError, SkeletonRows } from '../../ui/controls'
import { formatWhen } from '../../ui/when'
import { useSession } from '../auth/session'

const identitiesKey = ['auth', 'identities'] as const

const providerName: Record<Identity['provider'], string> = { google: 'Google' }

/**
 * The other ways into this account, and how to take one away.
 *
 * Unlink is drawn for every row but is only live when the account also has a
 * password: taking away the last way in would lock the person out of their own
 * files, and the server refuses it with a 409 anyway. Drawing it disabled with
 * the reason under it says why, where hiding it would leave somebody looking
 * for a control that is not there.
 *
 * And it asks first. The button sits where the session list's Revoke sits, at
 * the right edge of a row, in the same variant and size; a mis-aimed press has
 * no undo, and getting the link back means signing out and going round Google
 * again.
 */
export function IdentitiesSection() {
  const me = useSession()
  const client = useQueryClient()
  // The row being asked about, not its id: the question outlives the row on the
  // paths that drop it, and the dialog still has to name what it removed.
  const [confirming, setConfirming] = useState<Identity | null>(null)

  const identities = useQuery({ queryKey: identitiesKey, queryFn: listIdentities })

  // Dropped to, not refetched from, the server: the row is gone because the
  // DELETE said so — the same argument the session list makes.
  const dropRow = (id: string) =>
    client.setQueryData(identitiesKey, (was: Page<Identity> | undefined) =>
      was ? { ...was, items: was.items.filter((i) => i.id !== id) } : was,
    )

  const unlink = useMutation({
    mutationFn: unlinkIdentity,
    onSuccess: (_result, id) => {
      dropRow(id)
      setConfirming(null)
      toast.success('Sign-in method removed')
    },
    onError: (err: unknown, id) => {
      // A 404 is the link having gone between this list being fetched and the
      // click — unlinked in another tab, or on another device. The row
      // describes something that is already gone, so it goes as well, and the
      // question being asked about it goes with it: leaving the row there with
      // a live Unlink on it is the one outcome that is wrong whichever way the
      // person reads it.
      if (err instanceof ApiError && err.code === 'not_found') {
        dropRow(id)
        setConfirming(null)
        toast.success('That sign-in method was already removed')
        return
      }
      // Everything else keeps the dialog open with the server's own words in
      // it. The 409 — this is the account's last way in — is a refusal about
      // the row the question names, and closing on it would hide the answer
      // behind the screen the person just came from.
    },
  })

  const items = identities.data?.items ?? []

  return (
    <section aria-labelledby="identities-heading" className="flex flex-col gap-1.5">
      <h2 id="identities-heading" className="text-[15px] font-semibold text-ink">
        Sign-in methods
      </h2>
      <p className="text-[13px] text-ink-3">
        Accounts linked to this one. Signing in with a linked account signs you in here.
      </p>

      <div className="mt-4 overflow-hidden rounded-card border border-line bg-surface">
        {identities.isPending && <SkeletonRows rows={1} />}

        {identities.error && (
          <div role="alert" className="flex flex-col items-start gap-2 px-4 py-5">
            <p className="text-sm text-ink-2">The sign-in methods didn’t load.</p>
            <Button variant="outline" size="sm" onClick={() => void identities.refetch()}>
              Try again
            </Button>
          </div>
        )}

        {identities.isSuccess &&
          (items.length === 0 ? (
            <p className="px-4 py-5 text-[13px] text-ink-3">
              Nothing linked. You sign in with your email address and password.
            </p>
          ) : (
            <ul className="divide-y divide-line">
              {items.map((i) => (
                <li key={i.id} className="flex items-center gap-3 px-4 py-3">
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-[13px] font-medium text-ink">
                      {providerName[i.provider]} · {i.email_at_link}
                    </p>
                    <p className="mt-0.5 truncate text-[12px] text-ink-3">
                      linked {formatWhen(i.created_at)} · last used{' '}
                      {i.last_login_at ? formatWhen(i.last_login_at) : 'not since linking'}
                    </p>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={!me.has_password || (unlink.isPending && unlink.variables === i.id)}
                    onClick={() => {
                      // A refusal from a previous question is not an answer to
                      // this one.
                      unlink.reset()
                      setConfirming(i)
                    }}
                  >
                    Unlink
                  </Button>
                </li>
              ))}
            </ul>
          ))}
      </div>

      {items.length > 0 && !me.has_password && (
        <p className="mt-2 text-[13px] text-ink-3">
          This is the only way into your account. Set a password first, and unlinking becomes available.
        </p>
      )}

      <Dialog
        open={confirming !== null}
        onOpenChange={(open) => {
          if (!open) setConfirming(null)
        }}
      >
        <DialogContent className="sm:max-w-md">
          {confirming && (
            <>
              <DialogHeader>
                <DialogTitle>Unlink {providerName[confirming.provider]}?</DialogTitle>
                <DialogDescription>
                  Signing in with {providerName[confirming.provider]} as {confirming.email_at_link} will no longer
                  open this account — from then on it opens with its password only.
                </DialogDescription>
              </DialogHeader>
              {/* The one refusal worth showing in place rather than as a toast:
                  it says the row is still there and why, and the question it
                  answers is still on screen. */}
              <FormError error={unlink.error} />
              <DialogFooter>
                <Button variant="outline" onClick={() => setConfirming(null)}>
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  disabled={unlink.isPending}
                  onClick={() => unlink.mutate(confirming.id)}
                >
                  {unlink.isPending ? 'Unlinking…' : 'Unlink'}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </section>
  )
}
