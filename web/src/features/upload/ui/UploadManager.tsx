import { memo, useRef } from 'react'

import { secondaryButtonClass } from '../../../ui/controls'
import { formatBytes } from '../../../ui/format'
import type { UploadSnapshot } from '../engine/types'
import type { UploadActions } from './engineStore'
import { describeUpload } from './status'

/**
 * The upload manager: one row per upload, always visible, independent of which
 * folder is on screen. It is presentational — the container feeds it snapshots
 * and actions — so the whole surface is testable without a live engine.
 */
export function UploadManager({ items, actions }: { items: UploadSnapshot[]; actions: UploadActions }) {
  if (items.length === 0) return null
  const finished = items.some((i) => i.state === 'done' || i.state === 'canceled')

  return (
    <aside
      aria-label="Uploads"
      className="fixed bottom-4 right-4 max-h-[60vh] w-96 overflow-y-auto rounded-xl border border-neutral-200 bg-white shadow-lg"
    >
      <header className="flex items-center gap-2 border-b border-neutral-100 px-4 py-2">
        <h2 className="text-sm font-medium">Uploads</h2>
        {finished && (
          <button className={`ml-auto ${secondaryButtonClass}`} onClick={actions.clearFinished}>
            Clear finished
          </button>
        )}
      </header>
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
    </aside>
  )
}

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
    <li className="flex flex-col gap-1 border-b border-neutral-100 px-4 py-3 last:border-b-0">
      <div className="flex items-baseline gap-2">
        <span className="truncate text-sm font-medium">{props.name}</span>
        {props.renamed && (
          <span
            className="rounded bg-amber-100 px-1.5 py-0.5 text-xs text-amber-800"
            title={`Saved as “${props.name}” — “${props.original_name}” was already here`}
          >
            renamed
          </span>
        )}
        <span className="ml-auto shrink-0 text-xs text-neutral-500">{formatBytes(props.size)}</span>
      </div>

      <div className="h-1 w-full overflow-hidden rounded bg-neutral-100">
        <div
          role="progressbar"
          aria-valuenow={Math.round(props.progress * 100)}
          aria-label={`${props.name} progress`}
          className="h-full bg-neutral-900"
          style={{ width: `${Math.round(props.progress * 100)}%` }}
        />
      </div>

      <p className={`text-xs ${line.needsUser ? 'text-amber-700' : 'text-neutral-500'}`}>{line.text}</p>

      <div className="flex gap-2">
        {running && (
          <button className={secondaryButtonClass} onClick={() => actions.pause(id)}>
            Pause
          </button>
        )}
        {(state === 'paused' || state === 'queued') && (
          <button className={secondaryButtonClass} onClick={() => actions.resume(id)}>
            Resume
          </button>
        )}
        {(state === 'failed' || state === 'session_expired') && (
          <button className={secondaryButtonClass} onClick={() => actions.retry(id)}>
            Try again
          </button>
        )}
        {state === 'error_file_changed' && (
          <>
            <button className={secondaryButtonClass} onClick={() => repick.current?.click()}>
              Pick the file again
            </button>
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
          <button className={secondaryButtonClass} onClick={() => actions.cancel(id)}>
            Cancel
          </button>
        )}
      </div>
    </li>
  )
})
