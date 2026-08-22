import type { DriveNode } from '../../lib/api'
import { downloadHref } from '../../lib/api'
import { ghostButtonClass } from '../../ui/controls'
import { CloseIcon, DownloadIcon, TrashIcon } from '../../ui/icons'

/**
 * The command bar for whatever is selected.
 *
 * It only ever offers what the selection can actually do: Rename needs exactly
 * one item, Download needs exactly one file (the API has no archive endpoint,
 * and offering a button that silently downloads one of five would be a lie),
 * and Copy is files-only because the server answers a folder copy with 422.
 */
export function SelectionToolbar({
  chosen,
  busy,
  onClear,
  onRename,
  onMove,
  onCopy,
  onDelete,
}: {
  chosen: DriveNode[]
  busy: boolean
  onClear: () => void
  onRename: () => void
  onMove: () => void
  onCopy: () => void
  onDelete: () => void
}) {
  const single = chosen.length === 1 ? chosen[0] : null
  const files = chosen.filter((n) => n.kind === 'file')

  return (
    <div
      role="toolbar"
      aria-label="Selection actions"
      className="flex flex-wrap items-center gap-1 border-b border-line bg-teal-soft/50 px-2 py-2 sm:px-3"
    >
      <button className={ghostButtonClass} onClick={onClear} aria-label="Clear the selection">
        <CloseIcon />
      </button>
      <span className="numeric px-1 text-teal-strong">{chosen.length} selected</span>

      <span aria-hidden className="mx-1 h-4 w-px bg-line-strong" />

      {single?.kind === 'file' && (
        <a className={ghostButtonClass} href={downloadHref(single.id)} target="_blank" rel="noopener">
          <DownloadIcon />
          Download
        </a>
      )}
      {single && (
        <button className={ghostButtonClass} onClick={onRename} disabled={busy}>
          Rename
        </button>
      )}
      <button className={ghostButtonClass} onClick={onMove} disabled={busy}>
        Move to
      </button>
      {files.length > 0 && (
        <button className={ghostButtonClass} onClick={onCopy} disabled={busy}>
          Copy to
        </button>
      )}
      <button
        className={`${ghostButtonClass} hover:bg-danger-soft hover:text-danger`}
        onClick={onDelete}
        disabled={busy}
      >
        <TrashIcon />
        Delete
      </button>
    </div>
  )
}
