import { useQueryClient } from '@tanstack/react-query'
import { useRef, useState, type ReactNode } from 'react'
import { toast } from 'sonner'

import { buttonClass, secondaryButtonClass } from '../../../ui/controls'
import { FolderIcon, UploadIcon } from '../../../ui/icons'
import { childrenKey } from '../../browser/queries'
import { collectDropEntries, createFolderViaApi, ingest, walkEntries, walkFileList } from '../engine/traverse'
import type { DropItem, TraverseSink } from '../engine/traverse'
import { uploadActions } from './engineStore'

/**
 * Both ingress paths for files, wrapped around the folder listing.
 *
 * 1. drag-drop — `collectDropEntries` runs SYNCHRONOUSLY in the handler, before
 *    any await: `dataTransfer.items` is invalidated the moment the handler
 *    yields, and every entry read after that point comes back null.
 * 2. the `webkitdirectory` picker — the fallback for whole folders, and the one
 *    automation can drive (the day-0 spike measured that CDP-synthesized drops
 *    carry no directory entry at all).
 *
 * Both normalize into the same `ingest()` call, so neither path has logic of
 * its own that the other does not exercise.
 */
export function DropZone({ folderId, children }: { folderId: string; children: ReactNode }) {
  const [over, setOver] = useState(false)
  const filesInput = useRef<HTMLInputElement>(null)
  const folderInput = useRef<HTMLInputElement>(null)
  const client = useQueryClient()

  /**
   * Folders are created by `ingest` through a plain fetch inside the engine's
   * traverse module, which knows nothing about this cache. Without re-reading
   * the folder afterwards, a dropped tree is invisible until a manual reload —
   * and an EMPTY dropped folder, which enqueues no upload at all, is invisible
   * for good (the completion bridge only fires for files, and it invalidates
   * the file's own parent, which for a nested drop is the new subfolder).
   */
  const ingestInto = async (items: Parameters<typeof ingest>[0]) => {
    try {
      await ingest(items, folderId, sink)
    } finally {
      void client.invalidateQueries({ queryKey: childrenKey(folderId) })
    }
  }

  const sink: TraverseSink = {
    createFolder: createFolderViaApi,
    enqueueFile: (parentId, file) => uploadActions.enqueue(file, parentId),
    // One item failing must not discard the rest of a drop, so `ingest` keeps
    // going and reports here instead of rejecting.
    onError: (item: DropItem, err: unknown) =>
      toast.error(`Couldn't add ${item.kind === 'file' ? item.file.name : item.path.join('/')}`, {
        description: (err as Error)?.message,
      }),
  }

  return (
    <section
      onDragOver={(e) => {
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
        e.preventDefault()
        setOver(false)
        const entries = collectDropEntries(e.dataTransfer)
        void ingestInto(walkEntries(entries))
      }}
      className="relative flex flex-col gap-3 rounded-card"
      data-testid="drop-zone"
    >
      <div className="flex flex-wrap items-center gap-2">
        <button className={buttonClass} onClick={() => filesInput.current?.click()}>
          <UploadIcon />
          Upload files
        </button>
        <button className={secondaryButtonClass} onClick={() => folderInput.current?.click()}>
          <FolderIcon />
          Upload folder
        </button>
        <span className="text-[13px] text-ink-3">…or drop files and folders here</span>
        <input
          ref={filesInput}
          type="file"
          multiple
          aria-label="Upload files"
          className="hidden"
          onChange={(e) => {
            const files = e.target.files
            if (files) for (const file of files) uploadActions.enqueue(file, folderId)
            e.target.value = ''
          }}
        />
        <input
          ref={folderInput}
          type="file"
          aria-label="Upload folder"
          className="hidden"
          // Not in React's attribute types; the DOM property is what Chromium reads.
          {...{ webkitdirectory: '' }}
          onChange={(e) => {
            const files = e.target.files
            if (files) void ingestInto(walkFileList(files))
            e.target.value = ''
          }}
        />
      </div>
      {children}

      {/* The drop target says so while something is over it, and says nothing
          otherwise. `pointer-events-none` keeps the drop landing on the
          section underneath, which is what carries the handler. */}
      {over && (
        <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-card bg-accent-soft/80 ring-2 ring-accent">
          <span className="flex items-center gap-2 rounded-control bg-surface px-3 py-2 text-sm font-medium text-accent-strong shadow-card">
            <UploadIcon />
            Drop to upload here
          </span>
        </div>
      )}
    </section>
  )
}
