import { useQueryClient } from '@tanstack/react-query'
import { useRef, useState, type FormEvent } from 'react'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'

import { ApiError, type DriveNode, type Share, type ShareSettings } from '../../lib/api'
import { fieldClass, FormError, SkeletonRows } from '../../ui/controls'
import { formatUntil } from '../../ui/when'
import { isAcceptablePassword, passwordHint } from '../auth/password'
import { shareForNode, useShareForNode } from './queries'
import { useShareUrl } from './shareUrls'
import { copyShareUrl, useCreateShare, useShareCommands, useUpdateShareSettings } from './useShareCommands'

/**
 * One file's link: make it, copy it, replace it, change it, stop it.
 *
 * Keyed on the server's own answer to "is this file shared?" rather than on
 * anything the row knew, so two tabs cannot disagree about it. Four states:
 * no link (the form), a link whose URL this tab holds (the URL and Copy), a
 * link it does not (the facts and the actions), and the read having failed.
 *
 * The URL is shown once, here, and kept only in this tab's memory — the server
 * keeps a hash and cannot show it again. That is why the line under the input
 * says so, and why a tab that does not hold the URL is offered New link where
 * Copy would be, never a disabled Copy.
 */
export function ShareDialog({ node, onClose }: { node: DriveNode; onClose: () => void }) {
  const share = useShareForNode(node.id)
  const commands = useShareCommands()
  const [editing, setEditing] = useState(false)

  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open) onClose()
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Share "{node.name}"</DialogTitle>
            <DialogDescription>
              Anyone with the link can open the file. Stopping the link stops new downloads.
            </DialogDescription>
          </DialogHeader>

          {share.isPending && <SkeletonRows rows={1} />}

          {share.error != null && (
            <div className="flex flex-col items-start gap-2">
              <FormError error={share.error} />
              <Button variant="outline" size="sm" onClick={() => void share.refetch()}>
                Try again
              </Button>
            </div>
          )}

          {share.isSuccess && share.data === null && <CreateForm node={node} />}

          {share.isSuccess &&
            share.data !== null &&
            (editing ? (
              <SettingsForm share={share.data} onDone={() => setEditing(false)} />
            ) : (
              <Existing share={share.data} commands={commands} onEdit={() => setEditing(true)} />
            ))}
        </DialogContent>
      </Dialog>
      {commands.dialogs}
    </>
  )
}

/* ------------------------------------------------------------ the states */

function Existing({ share, commands, onEdit }: { share: Share; commands: ReturnType<typeof useShareCommands>; onEdit: () => void }) {
  const url = useShareUrl(share.id)

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1 text-[13px] text-ink-2">
        <p>
          {formatUntil(share.expires_at)} · Password {share.has_password ? 'on' : 'off'} · {downloads(share)}
        </p>
        {!share.node_live && (
          <p role="status" className="text-warn">
            In trash — the link is inert until you restore the file
          </p>
        )}
      </div>

      {url !== undefined && (
        <div className="flex flex-col gap-2">
          <LinkField url={url} />
          <p className="text-[13px] text-ink-3">
            Copy it now — Drive doesn't keep a copy. You can make a new link any time.
          </p>
        </div>
      )}

      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="outline" disabled={commands.busy} onClick={() => commands.newLink(share)}>
          New link
        </Button>
        <Button variant="outline" onClick={onEdit}>
          Settings
        </Button>
        <Button
          variant="outline"
          className="hover:bg-danger-soft hover:text-danger"
          disabled={commands.busy}
          onClick={() => commands.stopSharing(share)}
        >
          Stop sharing
        </Button>
      </div>
    </div>
  )
}

function downloads(share: Share): string {
  const n = share.download_count
  if (share.max_downloads !== null) return `${n} of ${share.max_downloads} downloads`
  return `${n} ${n === 1 ? 'download' : 'downloads'}`
}

/**
 * The URL, read-only, with Copy beside it.
 *
 * Where the clipboard is not there or refuses, the text is selected and the
 * button becomes a hint — the person copies it the way they would anywhere
 * else, and nothing has claimed to have done it for them.
 */
export function LinkField({ url }: { url: string }) {
  const input = useRef<HTMLInputElement>(null)
  const [manual, setManual] = useState(false)

  return (
    <div className="flex items-end gap-2">
      <label className={`${fieldClass} min-w-0 flex-1`}>
        Link
        <Input
          ref={input}
          readOnly
          value={url}
          spellCheck={false}
          onFocus={(e) => e.target.select()}
          className="numeric h-9 text-[12px]"
        />
      </label>
      {manual ? (
        <span className="shrink-0 pb-2 text-[13px] text-ink-3">Select and copy</span>
      ) : (
        <Button
          variant="outline"
          className="shrink-0"
          onClick={() =>
            void copyShareUrl(url).then((copied) => {
              if (copied) return
              setManual(true)
              input.current?.focus()
              input.current?.select()
            })
          }
        >
          Copy link
        </Button>
      )}
    </div>
  )
}

/* ------------------------------------------------------------- the forms */

type ExpiryPreset = '1d' | '7d' | '30d' | 'never' | 'date'

const PRESETS: { value: ExpiryPreset; label: string }[] = [
  { value: '1d', label: '1 day' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: 'never', label: 'Never' },
  { value: 'date', label: 'On a date' },
]

const DAY_MS = 24 * 60 * 60 * 1000

/** `YYYY-MM-DD` in local time, the shape a date input wants. */
function localDay(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

interface Fields {
  expiry: ExpiryPreset
  date: string
  password: string
  showPassword: boolean
  limit: string
}

/**
 * Turns the fields into the wire triple. A preset is counted from now, at the
 * moment of the submit; a chosen day lasts until the end of that day, which is
 * what a person picking "the 30th" means by it.
 */
function toSettings(fields: Fields): ShareSettings {
  let expires_at: string | null = null
  if (fields.expiry === '1d') expires_at = new Date(Date.now() + DAY_MS).toISOString()
  else if (fields.expiry === '7d') expires_at = new Date(Date.now() + 7 * DAY_MS).toISOString()
  else if (fields.expiry === '30d') expires_at = new Date(Date.now() + 30 * DAY_MS).toISOString()
  else if (fields.expiry === 'date' && fields.date !== '') {
    const [y, m, d] = fields.date.split('-').map(Number)
    expires_at = new Date(y, m - 1, d + 1).toISOString()
  }
  const limit = fields.limit.trim() === '' ? null : Number(fields.limit)
  return {
    expires_at,
    password: fields.password === '' ? null : fields.password,
    max_downloads: limit !== null && Number.isFinite(limit) ? Math.floor(limit) : null,
  }
}

function SettingsFields({
  fields,
  onChange,
  passwordBad,
  hasPassword,
}: {
  fields: Fields
  onChange: (next: Fields) => void
  passwordBad: boolean
  /** Editing a share that has one: an empty field turns it off, and says so. */
  hasPassword: boolean
}) {
  return (
    <>
      <label className={fieldClass}>
        Expires
        <select
          value={fields.expiry}
          onChange={(e) => onChange({ ...fields, expiry: e.target.value as ExpiryPreset })}
          className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:border-ring"
        >
          {PRESETS.map((p) => (
            <option key={p.value} value={p.value}>
              {p.label}
            </option>
          ))}
        </select>
      </label>
      {fields.expiry === 'date' && (
        <label className={fieldClass}>
          Expiry date
          <Input type="date" required value={fields.date} onChange={(e) => onChange({ ...fields, date: e.target.value })} />
        </label>
      )}

      <div className="flex flex-col gap-1.5">
        <label className={fieldClass}>
          Password
          <Input
            type={fields.showPassword ? 'text' : 'password'}
            autoComplete="off"
            aria-invalid={passwordBad || undefined}
            aria-describedby="share-password-hint"
            value={fields.password}
            onChange={(e) => onChange({ ...fields, password: e.target.value })}
          />
        </label>
        <label className="flex items-center gap-2 text-[13px] text-ink-2">
          <input
            type="checkbox"
            checked={fields.showPassword}
            onChange={(e) => onChange({ ...fields, showPassword: e.target.checked })}
          />
          Show password
        </label>
        {/* The rule, on screen before it is broken — the sign-up form's line. */}
        <p id="share-password-hint" className={`text-[13px] ${passwordBad ? 'text-danger' : 'text-ink-3'}`}>
          {hasPassword && fields.password === ''
            ? 'Leave it empty to turn the password off, or enter a new one.'
            : `Optional. ${passwordHint(fields.password)}.`}
        </p>
      </div>

      <label className={fieldClass}>
        Download limit
        <Input
          type="number"
          inputMode="numeric"
          min={1}
          max={1_000_000}
          step={1}
          placeholder="No limit"
          value={fields.limit}
          onChange={(e) => onChange({ ...fields, limit: e.target.value })}
        />
      </label>
    </>
  )
}

/** The fields judge themselves on submit, the way the sign-up form does. */
function usePasswordJudged(fields: Fields) {
  const [judged, setJudged] = useState(false)
  const bad = judged && fields.password !== '' && !isAcceptablePassword(fields.password)
  const acceptable = fields.password === '' || isAcceptablePassword(fields.password)
  return { bad, acceptable, judge: () => setJudged(true) }
}

function CreateForm({ node }: { node: DriveNode }) {
  const client = useQueryClient()
  const create = useCreateShare()
  const [fields, setFields] = useState<Fields>({ expiry: '7d', date: '', password: '', showPassword: false, limit: '' })
  const password = usePasswordJudged(fields)

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    password.judge()
    if (!password.acceptable) return
    create.mutate(
      { nodeId: node.id, settings: toSettings(fields) },
      {
        // A 409 is a link that already exists — made in another tab, or
        // between this dialog opening and the click. Re-reading puts that
        // link on screen in place of the form.
        onError: (err) => {
          if (err instanceof ApiError && err.code === 'exists') {
            void client.invalidateQueries({ queryKey: shareForNode(node.id) })
          }
        },
      },
    )
  }

  const exists = create.error instanceof ApiError && create.error.code === 'exists'

  return (
    <form noValidate onSubmit={onSubmit} className="flex flex-col gap-3">
      <SettingsFields fields={fields} onChange={setFields} passwordBad={password.bad} hasPassword={false} />
      {!exists && <FormError error={create.error} />}
      <div className="flex justify-end pt-1">
        <Button type="submit" disabled={create.isPending}>
          {create.isPending ? 'Creating…' : 'Create link'}
        </Button>
      </div>
    </form>
  )
}

function SettingsForm({ share, onDone }: { share: Share; onDone: () => void }) {
  const update = useUpdateShareSettings()
  const [fields, setFields] = useState<Fields>({
    expiry: share.expires_at === null ? 'never' : 'date',
    date: share.expires_at === null ? '' : localDay(share.expires_at),
    password: '',
    showPassword: false,
    limit: share.max_downloads === null ? '' : String(share.max_downloads),
  })
  const password = usePasswordJudged(fields)

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    password.judge()
    if (!password.acceptable) return
    // All three keys, every time: the server cannot tell "absent" from "clear",
    // so a body that left out the password would be refused, and one that
    // guessed would wipe it.
    update.mutate({ id: share.id, settings: toSettings(fields) }, { onSuccess: onDone })
  }

  return (
    <form noValidate onSubmit={onSubmit} className="flex flex-col gap-3">
      <SettingsFields
        fields={fields}
        onChange={setFields}
        passwordBad={password.bad}
        hasPassword={share.has_password}
      />
      <FormError error={update.error} />
      <div className="flex justify-end gap-2 pt-1">
        <Button type="button" variant="outline" onClick={onDone}>
          Cancel
        </Button>
        <Button type="submit" disabled={update.isPending}>
          {update.isPending ? 'Saving…' : 'Save settings'}
        </Button>
      </div>
    </form>
  )
}
