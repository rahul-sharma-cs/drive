import { formatBytes } from '../ui/format'
import { useUsage } from '../features/browser/queries'

/**
 * How much room is left, in the one place a person looks for it.
 *
 * The number counts the same bytes the upload path counts when it decides
 * whether to accept a file — published files including trashed ones, plus
 * uploads still in flight. A meter that disagreed with the refusal would be
 * worse than no meter, which is why it reads the server rather than adding up
 * what happens to be on screen.
 */
export function StorageMeter() {
  const usage = useUsage()
  if (!usage.data) return null

  const { used, quota } = usage.data
  if (quota === null) {
    return <p className="numeric px-2 text-ink-3">{formatBytes(used)} stored</p>
  }

  const fraction = Math.min(1, used / quota)
  const tight = fraction >= 0.9

  return (
    <div className="px-2">
      <div
        role="progressbar"
        aria-valuenow={Math.round(fraction * 100)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label="Storage used"
        className="h-1.5 w-full overflow-hidden rounded-full bg-line"
      >
        <div
          className={`h-full rounded-full transition-[width] duration-[400ms] ease-out ${tight ? 'bg-warn' : 'bg-teal'}`}
          style={{ width: `${Math.max(2, fraction * 100)}%` }}
        />
      </div>
      <p className={`numeric mt-1.5 ${tight ? 'text-warn' : 'text-ink-3'}`}>
        {formatBytes(used)} of {formatBytes(quota)} used
      </p>
    </div>
  )
}
