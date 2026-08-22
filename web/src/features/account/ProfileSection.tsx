import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { updateMe } from '../../lib/api'
import { FormError } from '../../ui/controls'
import { useSession, useSetSession } from '../auth/session'

/**
 * The name the product calls you by, and the address it can't change.
 *
 * The saved name is written straight into the `me` cache from the PATCH's own
 * response rather than invalidated: the avatar in the top bar reads that cache,
 * and a refetch would both blank the initials for a frame and spend a request
 * on an answer the server has already given.
 */
export function ProfileSection() {
  const user = useSession()
  const setSession = useSetSession()
  const [name, setName] = useState(user.display_name)

  const save = useMutation({
    mutationFn: () => updateMe(name.trim()),
    onSuccess: (updated) => {
      setSession(updated)
      setName(updated.display_name)
      toast.success('Name updated')
    },
  })

  const trimmed = name.trim()
  const unchanged = trimmed === user.display_name

  return (
    <section aria-labelledby="profile-heading" className="flex flex-col gap-1.5">
      <h2 id="profile-heading" className="text-[15px] font-semibold text-ink">
        Profile
      </h2>
      <p className="text-[13px] text-ink-3">How Drive addresses you, and which mailbox the account belongs to.</p>

      <form
        className="mt-4 flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          save.mutate()
        }}
      >
        <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-2">
          Display name
          <Input
            name="display_name"
            autoComplete="name"
            required
            // The server's own cap. It counts runes and this counts UTF-16
            // units, so a name full of astral characters stops a little early —
            // the direction that refuses a name the server would have taken,
            // never the one that invites a name it would refuse.
            maxLength={100}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>

        <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-2">
          Email
          <Input
            name="email"
            type="email"
            readOnly
            value={user.email}
            className="bg-surface-muted text-ink-3"
            // Read-only rather than disabled: the address still has to be
            // selectable and reachable by a screen reader — it is the one piece
            // of identity a person may need to copy out.
            aria-describedby="email-note"
          />
        </label>
        <p id="email-note" className="-mt-2 text-[12px] text-ink-3">
          Changing the address on an account isn’t supported yet.
        </p>

        <FormError error={save.error} />

        <div>
          <Button type="submit" disabled={save.isPending || trimmed === '' || unchanged}>
            {save.isPending ? 'Saving…' : 'Save name'}
          </Button>
        </div>
      </form>
    </section>
  )
}
