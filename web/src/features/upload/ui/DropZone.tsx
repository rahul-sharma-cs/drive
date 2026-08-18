import { useRef, useState, type ReactNode } from 'react'
import { toast } from 'sonner'

import { secondaryButtonClass } from '../../../ui/controls'
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
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setOver(false)
        const entries = collectDropEntries(e.dataTransfer)
        void ingest(walkEntries(entries), folderId, sink)
      }}
      className={`flex flex-col gap-3 rounded-xl ${over ? 'outline-2 outline-dashed outline-neutral-400' : ''}`}
      data-testid="drop-zone"
    >
      <div className="flex items-center gap-2">
        <button className={secondaryButtonClass} onClick={() => filesInput.current?.click()}>
          Upload files
        </button>
        <button className={secondaryButtonClass} onClick={() => folderInput.current?.click()}>
          Upload folder
        </button>
        <span className="text-xs text-neutral-500">…or drop files and folders here</span>
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
            if (files) void ingest(walkFileList(files), folderId, sink)
            e.target.value = ''
          }}
        />
      </div>
      {children}
    </section>
  )
}
