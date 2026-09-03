import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'

import { ApiError, type DriveNode, type Share, type ShareSettingsPatch } from '../../lib/api'
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
 * no link (the form), a link whose URL this browser holds (the URL and Copy),
 * a link it does not (the facts and the actions), and the read having failed.
 *
 * The URL is shown once, here, and kept only in this browser — the server
 * keeps a hash and cannot show it again. That is why the line under the input
 * says so, and why a browser that does not hold the URL is told so and offered
 * New link where Copy would be, never a disabled Copy.
 */

/** Under the facts, wherever this browser has no URL for a link that exists. */
export const LINK_NOT_KEPT = 'Link not kept in this browser — copy it where you made it, or make a new one.'
/** All the dialog needs of the file — a row's `DriveNode` and a share's `ShareNode` both carry it. */
type Named = Pick<DriveNode, 'id' | 'name'>

export function ShareDialog({ node, onClose }: { node: Named; onClose: () => void }) {
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

      {url !== undefined ? (
        <div className="flex flex-col gap-2">
          <LinkField key={url} url={url} />
          <p className="text-[13px] text-ink-3">
            Drive keeps this link only in this browser — copy it somewhere safe. You can make a new link any time.
          </p>
        </div>
      ) : (
        <p className="text-[13px] text-ink-3">{LINK_NOT_KEPT}</p>
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

/** How long the button says Copied before it offers Copy link again. */
const COPIED_MS = 2_000

/**
 * The URL, read-only, with Copy beside it.
 *
 * A copy that landed says so on the button itself — Copied, for two seconds —
 * as well as in the toast, and the field is a polite live region so the
 * change is announced. Where the clipboard is not there or refuses, the text
 * is selected and the button becomes a hint — the person copies it the way
 * they would anywhere else, and nothing has claimed to have done it for them.
 *
 * Both callers key it on the URL, so a replaced link starts over: neither
 * Copied nor the hint stands beside a URL nobody has copied.
 */
export function LinkField({ url }: { url: string }) {
  const input = useRef<HTMLInputElement>(null)
  const [manual, setManual] = useState(false)
  const [copied, setCopied] = useState(false)
  const revert = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(revert.current), [])

  return (
    <div aria-live="polite" className="flex items-end gap-2">
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
            void copyShareUrl(url).then((landed) => {
              if (landed) {
                setCopied(true)
                clearTimeout(revert.current)
                revert.current = setTimeout(() => setCopied(false), COPIED_MS)
                return
              }
              setManual(true)
              input.current?.focus()
              input.current?.select()
            })
          }
        >
          {copied ? 'Copied' : 'Copy link'}
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
 * The two wire fields that are always sent. A preset is counted from now, at
 * the moment of the submit; a chosen day lasts until the end of that day,
 * which is what a person picking "the 30th" means by it. The password is not
 * here — it reaches the wire only when the person acted on it.
 */
function toSettings(fields: Fields): { expires_at: string | null; max_downloads: number | null } {
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
    max_downloads: limit !== null && Number.isFinite(limit) ? Math.floor(limit) : null,
  }
}

function SettingsFields({
  fields,
  onChange,
  password,
}: {
  fields: Fields
  onChange: (next: Fields) => void
  /** The password control: a plain optional field on create, the tri-state on settings. */
  password: ReactNode
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

      {password}

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

function PasswordField({
  fields,
  onChange,
  bad,
  hint,
}: {
  fields: Fields
  onChange: (next: Fields) => void
  bad: boolean
  hint: string
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label className={fieldClass}>
        Password
        <Input
          type={fields.showPassword ? 'text' : 'password'}
          autoComplete="off"
          aria-invalid={bad || undefined}
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
      <p id="share-password-hint" className={`text-[13px] ${bad ? 'text-danger' : 'text-ink-3'}`}>
        {hint}
      </p>
    </div>
  )
}

/** The fields judge themselves on submit, the way the sign-up form does. */
function usePasswordJudged(fields: Fields) {
  const [judged, setJudged] = useState(false)
  const bad = judged && fields.password !== '' && !isAcceptablePassword(fields.password)
  const acceptable = fields.password === '' || isAcceptablePassword(fields.password)
  return { bad, acceptable, judge: () => setJudged(true) }
}

function CreateForm({ node }: { node: Named }) {
  const client = useQueryClient()
  const create = useCreateShare()
  const [fields, setFields] = useState<Fields>({ expiry: '7d', date: '', password: '', showPassword: false, limit: '' })
  const password = usePasswordJudged(fields)

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    password.judge()
    if (!password.acceptable) return
    create.mutate(
      // All four keys, null for "none": a create has no current state to keep.
      { nodeId: node.id, settings: { ...toSettings(fields), password: fields.password === '' ? null : fields.password } },
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
      <SettingsFields
        fields={fields}
        onChange={setFields}
        password={
          <PasswordField
            fields={fields}
            onChange={setFields}
            bad={password.bad}
            hint={`Optional. ${passwordHint(fields.password)}.`}
          />
        }
      />
      {!exists && <FormError error={create.error} />}
      <div className="flex justify-end pt-1">
        <Button type="submit" disabled={create.isPending}>
          {create.isPending ? 'Creating…' : 'Create link'}
        </Button>
      </div>
    </form>
  )
}

/** What the person has said about the password so far. Untouched means untouched. */
type PasswordAction = 'keep' | 'clear' | 'set'

function SettingsForm({ share, onDone }: { share: Share; onDone: () => void }) {
  const update = useUpdateShareSettings()
  const [fields, setFields] = useState<Fields>({
    expiry: share.expires_at === null ? 'never' : 'date',
    date: share.expires_at === null ? '' : localDay(share.expires_at),
    password: '',
    showPassword: false,
    limit: share.max_downloads === null ? '' : String(share.max_downloads),
  })
  // Mirrors the wire's tri-state: absent keeps, null clears, a string sets.
  // A link with no password starts at 'set', where an empty field still sends
  // nothing — the field was never acted on.
  const [action, setAction] = useState<PasswordAction>(share.has_password ? 'keep' : 'set')
  const [judged, setJudged] = useState(false)
  // Whether the expiry control itself was touched. The key is always sent, but
  // an untouched expiry goes back byte-for-byte: re-deriving end-of-day from
  // the prefilled date would quietly extend a link that expires mid-day, every
  // time any other setting was saved.
  const [expiryEdited, setExpiryEdited] = useState(false)

  const change = (next: Fields) => {
    if (next.expiry !== fields.expiry || next.date !== fields.date) setExpiryEdited(true)
    setFields(next)
  }

  const typing = action === 'set' && fields.password !== ''
  const bad = judged && typing && !isAcceptablePassword(fields.password)

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    setJudged(true)
    if (typing && !isAcceptablePassword(fields.password)) return
    // Expiry and cap always; the password key only when acted on. Absent is
    // "keep", which is what lets an expiry change leave a password standing
    // that nobody can re-type.
    const settings: ShareSettingsPatch = toSettings(fields)
    if (!expiryEdited) settings.expires_at = share.expires_at
    if (action === 'clear') settings.password = null
    else if (typing) settings.password = fields.password
    update.mutate({ id: share.id, settings }, { onSuccess: onDone })
  }

  const keep = (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={() => {
        setAction('keep')
        setFields({ ...fields, password: '' })
      }}
    >
      Keep password
    </Button>
  )

  const password =
    action === 'keep' ? (
      <div className={fieldClass}>
        Password
        <div className="flex flex-wrap items-center gap-2">
          <p className="font-normal text-ink-2">Password is on</p>
          <Button type="button" variant="outline" size="sm" onClick={() => setAction('set')}>
            Change password
          </Button>
          <Button type="button" variant="outline" size="sm" onClick={() => setAction('clear')}>
            Remove password
          </Button>
        </div>
      </div>
    ) : action === 'clear' ? (
      <div className={fieldClass}>
        Password
        <div className="flex flex-wrap items-center gap-2">
          <p className="font-normal text-ink-2">Comes off when you save.</p>
          {keep}
        </div>
      </div>
    ) : (
      <div className="flex flex-col gap-1.5">
        <PasswordField
          fields={fields}
          onChange={change}
          bad={bad}
          hint={
            share.has_password
              ? `${passwordHint(fields.password)}. Leave it empty to keep the current one.`
              : `Optional. ${passwordHint(fields.password)}.`
          }
        />
        {share.has_password && <div>{keep}</div>}
      </div>
    )

  return (
    <form noValidate onSubmit={onSubmit} className="flex flex-col gap-3">
      <SettingsFields fields={fields} onChange={change} password={password} />
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
