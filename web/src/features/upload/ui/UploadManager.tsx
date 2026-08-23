import { ChevronDown } from 'lucide-react'
import { memo, useRef, useState, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import { formatBytes } from '../../../ui/format'
import type { UploadSnapshot, UploadState } from '../engine/types'
import type { UploadActions } from './engineStore'
import { describeUpload } from './status'

/**
 * The upload manager: one row per upload, always visible, independent of which
 * folder is on screen. It is presentational — the container feeds it snapshots
 * and actions — so the whole surface is testable without a live engine.
 *
 * It sits on the page as a floating translucent layer rather than an opaque
 * panel bolted to the corner: the file list stays readable underneath it, which
 * matters because the thing you are usually doing while an upload runs is
 * looking at the folder it is landing in.
 */
export function UploadManager({
  items,
  actions,
  resumable = null,
}: {
  items: UploadSnapshot[]
  actions: UploadActions
  /** Interrupted server sessions waiting for their file to be picked again. */
  resumable?: ReactNode
}) {
  const [open, setOpen] = useState(true)
  if (items.length === 0 && resumable === null) return null
  const finished = items.some((i) => i.state === 'done' || i.state === 'canceled')

  return (
    <aside
      aria-label="Uploads"
      // Positioned by the dock stack in the layout, not by itself: it is one
      // panel in that corner rather than the only thing entitled to it.
      className="dock-enter material pointer-events-auto flex max-h-[70vh] w-full flex-col overflow-hidden
                 rounded-pop border border-line shadow-dock"
    >
      <header className="flex items-center gap-2 border-b border-line px-4 py-2.5">
        <div className="min-w-0">
          <h2 className="text-[13px] font-semibold text-ink">Uploads</h2>
          <p className="numeric truncate text-ink-3">{summarize(items)}</p>
        </div>
        <div className="ml-auto flex items-center gap-1">
          {finished && (
            <Button variant="ghost" size="sm" onClick={actions.clearFinished}>
              Clear finished
            </Button>
          )}
          <Button
            variant="ghost"
            size="icon-sm"
            aria-expanded={open}
            aria-label={open ? 'Hide upload details' : 'Show upload details'}
            onClick={() => setOpen((was) => !was)}
          >
            <ChevronDown className={`transition-transform duration-200 ${open ? '' : 'rotate-180'}`} />
          </Button>
        </div>
      </header>
      {open && (
        <div className="overflow-y-auto overscroll-contain">
          {resumable}
          <ul>
            {items.map((item) => (
              // Spread, not the object: memo compares props shallowly, and
              // `getSnapshot` re-mints every row object whenever anything changes.
              <UploadRow
                key={item.id}
                id={item.id}
                name={item.name}
                original_name={item.original_name}
                renamed={item.renamed}
                size={item.size}
                state={item.state}
                progress={item.progress}
                parts_confirmed={item.parts_confirmed}
                parts_total={item.parts_total}
                speed_bps={item.speed_bps}
                eta_seconds={item.eta_seconds}
                error={item.error}
                actions={actions}
              />
            ))}
          </ul>
        </div>
      )}
    </aside>
  )
}

/** One line for the whole dock, so a collapsed dock still says what it is doing. */
function summarize(items: UploadSnapshot[]): string {
  if (items.length === 0) return 'Waiting for a file'
  const running = items.filter((i) => RUNNING.has(i.state)).length
  const waiting = items.filter((i) => WAITING.has(i.state)).length
  const done = items.filter((i) => i.state === 'done').length
  const parts: string[] = []
  if (running > 0) parts.push(`${running} uploading`)
  if (waiting > 0) parts.push(`${waiting} waiting on you`)
  if (done > 0) parts.push(`${done} finished`)
  return parts.length > 0 ? parts.join(' · ') : `${items.length} in the list`
}

const RUNNING = new Set<UploadState>(['preparing', 'verifying', 'uploading', 'completing'])
const WAITING = new Set<UploadState>(['conflict', 'failed', 'session_expired', 'error_file_changed', 'paused'])

type RowProps = Pick<
  UploadSnapshot,
  | 'id'
  | 'name'
  | 'original_name'
  | 'renamed'
  | 'size'
  | 'state'
  | 'progress'
  | 'parts_confirmed'
  | 'parts_total'
  | 'speed_bps'
  | 'eta_seconds'
  | 'error'
> & { actions: UploadActions }

const UploadRow = memo(function UploadRow(props: RowProps) {
  const { id, state, actions } = props
  const line = describeUpload(props)
  const repick = useRef<HTMLInputElement>(null)
  const running = state === 'preparing' || state === 'verifying' || state === 'uploading' || state === 'completing'
  const done = state === 'done' || state === 'canceled'

  return (
    <li className="flex flex-col gap-2 border-b border-line px-4 py-3 last:border-b-0">
      <div className="flex items-baseline gap-2">
        <span className="min-w-0 truncate text-[13px] font-medium text-ink">{props.name}</span>
        {props.renamed && (
          <span
            className="shrink-0 rounded bg-warn-soft px-1.5 py-0.5 text-[11px] font-medium text-warn"
            title={`Saved as “${props.name}” — “${props.original_name}” was already here`}
          >
            renamed
          </span>
        )}
        <span className="numeric ml-auto shrink-0 text-ink-3">{formatBytes(props.size)}</span>
      </div>

      <PartMeter
        name={props.name}
        state={state}
        progress={props.progress}
        confirmed={props.parts_confirmed}
        total={props.parts_total}
      />

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <p className={`numeric ${line.needsUser ? 'text-warn' : 'text-ink-3'}`}>{line.text}</p>

        <div className="ml-auto flex shrink-0 gap-1">
          {running && (
            <Button variant="ghost" size="sm" onClick={() => actions.pause(id)}>
              Pause
            </Button>
          )}
          {(state === 'paused' || state === 'queued') && (
            <Button variant="outline" size="sm" onClick={() => actions.resume(id)}>
              Resume
            </Button>
          )}
          {(state === 'failed' || state === 'session_expired') && (
            <Button variant="outline" size="sm" onClick={() => actions.retry(id)}>
              Try again
            </Button>
          )}
          {state === 'error_file_changed' && (
            <>
              <Button variant="outline" size="sm" onClick={() => repick.current?.click()}>
                Pick the file again
              </Button>
              <input
                ref={repick}
                type="file"
                aria-label={`Pick ${props.original_name} again`}
                className="hidden"
                onChange={(e) => {
                  const file = e.target.files?.[0]
                  if (file) actions.reselect(id, file)
                  e.target.value = ''
                }}
              />
            </>
          )}
          {!done && (
            <Button
              variant="ghost"
              size="sm"
              className="hover:bg-danger-soft hover:text-danger"
              onClick={() => actions.cancel(id)}
            >
              Cancel
            </Button>
          )}
        </div>
      </div>
    </li>
  )
})

/**
 * Progress, drawn the way this product actually measures it: one segment per
 * multipart part, filled as the server confirms it. That is not decoration —
 * a confirmed part is a part that survives a crash, and the segments that are
 * already lit when a resumed upload appears are exactly the bytes nobody has
 * to send again.
 *
 * Above ~48 parts the segments would be thinner than the gaps between them, so
 * a large file falls back to a continuous bar carrying the same fraction.
 */
function PartMeter({
  name,
  state,
  progress,
  confirmed,
  total,
}: {
  name: string
  state: UploadState
  progress: number
  confirmed: number
  total: number
}) {
  const pct = Math.round(progress * 100)
  const fill = METER_FILL[state] ?? 'bg-teal'
  const segmented = total > 1 && total <= 48

  return (
    <div
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-label={`${name} progress`}
      className="flex h-1.5 w-full gap-px overflow-hidden rounded-full"
    >
      {segmented ? (
        Array.from({ length: total }, (_, i) => (
          <span
            key={i}
            className={`h-full flex-1 first:rounded-l-full last:rounded-r-full transition-colors duration-200 ${
              i < confirmed ? fill : 'bg-line'
            }`}
          />
        ))
      ) : (
        <span className="h-full w-full overflow-hidden rounded-full bg-line">
          <span
            className={`block h-full rounded-full transition-[width] duration-300 ease-out ${fill}`}
            style={{ width: `${pct}%` }}
          />
        </span>
      )}
    </div>
  )
}

/** Colour is the state: in motion, waiting on you, or failed. */
const METER_FILL: Partial<Record<UploadState, string>> = {
  paused: 'bg-warn',
  paused_offline: 'bg-warn',
  paused_backend: 'bg-warn',
  conflict: 'bg-warn',
  session_expired: 'bg-warn',
  error_file_changed: 'bg-warn',
  failed: 'bg-danger',
  canceled: 'bg-ink-3',
}
