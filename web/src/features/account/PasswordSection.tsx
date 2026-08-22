import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { changePassword } from '../../lib/api'
import { FormError } from '../../ui/controls'
import { isAcceptablePassword, passwordHint } from '../auth/password'
import { sessionsKey } from './SessionsSection'

/**
 * Change the password.
 *
 * The confirm field is checked here rather than at the server: a typo in the
 * repeat is not the server's business, and sending it would spend the change
 * budget — the endpoint reaches Argon2 and is rate-limited for that reason — on
 * a request that could never have succeeded.
 */
export function PasswordSection() {
  const client = useQueryClient()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [mismatch, setMismatch] = useState(false)
  const [nextJudged, setNextJudged] = useState(false)

  const nextBad = nextJudged && !isAcceptablePassword(next)

  const change = useMutation({
    mutationFn: () => changePassword(current, next),
    onSuccess: () => {
      setCurrent('')
      setNext('')
      setConfirm('')
      setNextJudged(false)
      // The server revoked every other session inside the same transaction, so
      // the list below this form is now describing devices that no longer have
      // a session — with a live Revoke button on each. It has to be re-read, not
      // patched: which rows went is the server's answer, not this form's.
      void client.invalidateQueries({ queryKey: sessionsKey })
      toast.success('Password changed')
    },
  })

  return (
    <section aria-labelledby="password-heading" className="flex flex-col gap-1.5">
      <h2 id="password-heading" className="text-[15px] font-semibold text-ink">
        Password
      </h2>
      <p className="text-[13px] text-ink-3">
        Changing it signs out every other device — this one stays signed in.
      </p>

      <form
        className="mt-4 flex flex-col gap-4"
        // No `noValidate` here, unlike the signed-out forms: this form has a
        // "current password" field, and letting an empty one through to the
        // server would spend the change budget -- the endpoint reaches Argon2
        // twice and is rate-limited for it -- on a request that cannot succeed.
        // The browser's own required check is doing useful work; the length
        // rule below is about a password that is filled in and still too short.
        onSubmit={(e) => {
          e.preventDefault()
          // The length rule first: a new password too short to be accepted is
          // worth saying so about even if the repeat below it also disagrees.
          setNextJudged(true)
          if (!isAcceptablePassword(next)) return
          if (next !== confirm) {
            setMismatch(true)
            change.reset()
            return
          }
          setMismatch(false)
          change.mutate()
        }}
      >
        <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-2">
          Current password
          <Input
            type="password"
            name="current_password"
            autoComplete="current-password"
            required
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </label>

        <div className="flex flex-col gap-1.5">
          <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-2">
            New password
            <Input
              type="password"
              name="new_password"
              autoComplete="new-password"
              required
              aria-invalid={nextBad || undefined}
              aria-describedby="account-password-hint"
              value={next}
              onChange={(e) => {
                setNext(e.target.value)
                setMismatch(false)
              }}
              onBlur={() => {
                if (next !== '') setNextJudged(true)
              }}
            />
          </label>
          {/* Outside the label on purpose: inside it, this line would be read
              out as part of the field's name. The Input already carries its own
              aria-invalid treatment, so only the colour is set here. */}
          <p
            id="account-password-hint"
            className={`text-[13px] ${nextBad ? 'text-danger' : 'text-ink-3'}`}
          >
            {passwordHint(next)}
          </p>
        </div>

        <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-2">
          Confirm new password
          <Input
            type="password"
            name="confirm_password"
            autoComplete="new-password"
            required
            value={confirm}
            onChange={(e) => {
              setConfirm(e.target.value)
              setMismatch(false)
            }}
          />
        </label>

        {mismatch ? (
          <p
            role="alert"
            className="rounded-control bg-danger-soft px-3 py-2 text-[13px] text-danger"
          >
            Those two passwords don’t match.
          </p>
        ) : (
          <FormError error={change.error} />
        )}

        <div>
          <Button type="submit" disabled={change.isPending}>
            {change.isPending ? 'Changing…' : 'Change password'}
          </Button>
        </div>
      </form>
    </section>
  )
}
