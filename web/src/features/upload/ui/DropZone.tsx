import { useState, type ReactNode } from 'react'

import { useCurrentFolder } from '../../../app/CurrentFolder'
import { UploadIcon } from '../../../ui/icons'
import { collectDropEntries, walkEntries } from '../engine/traverse'
import { useIngest } from './pickers'

/**
 * True when a drag carries files from outside the browser. An internal drag —
 * a row on its way into another folder — carries our own MIME type instead,
 * and must be left alone so the folder rows can handle it.
 */
export function isFileDrag(dt: DataTransfer | null): boolean {
  return dt !== null && Array.from(dt.types).includes('Files')
}

/**
 * Drag-and-drop ingress, wrapped around the folder listing.
 *
 * `collectDropEntries` runs SYNCHRONOUSLY in the handler, before any await:
 * `dataTransfer.items` is invalidated the moment the handler yields, and every
 * entry read after that point comes back null.
 *
 * The picker half of ingress lives in `pickers.tsx` now, mounted once by the
 * layout so the rail's New menu can reach it from any screen. Both paths
 * normalize into the same `ingest()` call through `useIngest`, so neither has
 * logic of its own that the other does not exercise.
 */
export function DropZone({ children }: { children: ReactNode }) {
  const [over, setOver] = useState(false)
  const folderId = useCurrentFolder()
  const ingestInto = useIngest()

  return (
    <section
      onDragOver={(e) => {
        // Only a drag carrying FILES is an upload. A row being dragged inside
        // the app is a move, and it used to light this whole panel up with
        // "Drop to upload here" — offering to upload a file that is already
        // uploaded. `types` is the one part of dataTransfer readable during a
        // dragover (the payload is not), which is exactly why the decision is
        // made on it.
        if (!isFileDrag(e.dataTransfer)) return
        e.preventDefault()
        setOver(true)
      }}
      // `dragleave` also fires when the pointer crosses from this element onto
      // one of its own children, so the highlight is only dropped once the
      // pointer has actually left the subtree — otherwise it strobes over
      // every row it passes.
      onDragLeave={(e) => {
        const next = e.relatedTarget
        if (next instanceof Node && e.currentTarget.contains(next)) return
        setOver(false)
      }}
      onDrop={(e) => {
        if (!isFileDrag(e.dataTransfer)) return
        e.preventDefault()
        setOver(false)
        const entries = collectDropEntries(e.dataTransfer)
        void ingestInto(walkEntries(entries), folderId)
      }}
      className="group/zone relative flex flex-col gap-3 rounded-card"
      data-testid="drop-zone"
    >
      {children}

      {/* A full folder takes drops just as an empty one does, but the empty
          state's invitation leaves with the first file and nothing else on the
          screen says a drag would land here. Shown only once there are rows,
          so it never repeats what the empty state is already saying. */}
      <p className="hidden px-1 text-[12px] text-ink-3 group-has-[[data-testid=file-row]]/zone:block">
        Drag files or folders in from your computer to upload them here.
      </p>

      {/* The drop target says so while something is over it, and says nothing
          otherwise. `pointer-events-none` keeps the drop landing on the
          section underneath, which is what carries the handler. */}
      {over && (
        <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-card bg-teal-soft/80 ring-2 ring-teal">
          <span className="flex items-center gap-2 rounded-control bg-surface px-3 py-2 text-sm font-medium text-teal-strong shadow-card">
            <UploadIcon />
            Drop to upload here
          </span>
        </div>
      )}
    </section>
  )
}
