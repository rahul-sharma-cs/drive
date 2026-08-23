import { Button } from '@/components/ui/button'
import { formatBytes } from '../../ui/format'
import { IndividualDownloads } from './IndividualDownloads'
import { cancelZipDownload, useZipJob, type ZipJob } from './useZipDownload'

/**
 * The archive's two possible screens: the dock while one is being built, and
 * the per-file offer when this browser cannot build it at all.
 *
 * They ship together because they are mounted together — the layout puts this
 * one component in the corner, and the offer (a dialog, portalled to the body
 * from wherever it is rendered) has no separate home in the tree. Only one of
 * the two is ever up: the offer replaces the archive rather than reporting on
 * it.
 */
export function ZipDock() {
  const job = useZipJob()

  return (
    <>
      {job !== null && <ArchivePanel job={job} />}
      <IndividualDownloads />
    </>
  )
}

/**
 * What the app is doing while it builds an archive.
 *
 * Same panel as the upload manager, in the same corner, stacked above it —
 * both are background work reporting from the bottom right, and two panels
 * that styled themselves differently would read as two products. Positioning
 * belongs to the layout's `DockStack`, not to either panel.
 *
 * It says the size before it says the percentage: while the walk is still
 * running the total is not known yet, and a bar that sat at 0% through a long
 * paginated walk would look stuck rather than busy.
 */
function ArchivePanel({ job }: { job: ZipJob }) {
  const walking = job.total === 0
  const pct = walking ? 0 : Math.min(100, Math.round((job.written / job.total) * 100))

  return (
    <aside
      aria-label="Archive"
      className="dock-enter material pointer-events-auto flex w-full flex-col overflow-hidden
                 rounded-pop border border-line shadow-dock"
    >
      <header className="flex items-center gap-2 border-b border-line px-4 py-2.5">
        <div className="min-w-0">
          <h2 className="truncate text-[13px] font-semibold text-ink">{job.name}</h2>
          <p className="numeric truncate text-ink-3">
            {walking ? 'Looking through the folders…' : `${formatBytes(job.written)} of ${formatBytes(job.total)}`}
          </p>
        </div>
        <Button
          variant="ghost"
          size="sm"
          className="ml-auto shrink-0 hover:bg-danger-soft hover:text-danger"
          onClick={cancelZipDownload}
        >
          Cancel
        </Button>
      </header>

      <div className="flex flex-col gap-2 px-4 py-3">
        <div
          role="progressbar"
          aria-valuenow={pct}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={`${job.name} progress`}
          className="h-1.5 w-full overflow-hidden rounded-full bg-line"
        >
          <span
            className="block h-full rounded-full bg-teal transition-[width] duration-300 ease-out"
            style={{ width: `${pct}%` }}
          />
        </div>
        {job.current !== '' && <p className="numeric truncate text-ink-3">{job.current}</p>}
      </div>
    </aside>
  )
}
