import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'

import { listIdentities, unlinkIdentity, type Identity, type Page } from '../../lib/api'
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
 */
export function IdentitiesSection() {
  const me = useSession()
  const client = useQueryClient()

  const identities = useQuery({ queryKey: identitiesKey, queryFn: listIdentities })

  const unlink = useMutation({
    mutationFn: unlinkIdentity,
    onSuccess: (_result, id) => {
      // Dropped to, not refetched from, the server: the row is gone because the
      // DELETE said so — the same argument the session list makes.
      client.setQueryData(identitiesKey, (was: Page<Identity> | undefined) =>
        was ? { ...was, items: was.items.filter((i) => i.id !== id) } : was,
      )
      toast.success('Sign-in method removed')
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
                    onClick={() => unlink.mutate(i.id)}
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

      {/* The one refusal worth showing in place rather than as a toast: it says
          the row is still there and why, and the row is still on screen. */}
      <div className="mt-2 empty:mt-0">
        <FormError error={unlink.error} />
      </div>
    </section>
  )
}
