import { useQueryClient } from '@tanstack/react-query'
import { useRef } from 'react'
import { toast } from 'sonner'

import { useCurrentFolder } from '../../../app/CurrentFolder'
import { childrenKey } from '../../browser/queries'
import { createFolderViaApi, ingest, walkFileList } from '../engine/traverse'
import type { DropItem, TraverseSink } from '../engine/traverse'
import { uploadActions } from './engineStore'

type PickerKind = 'files' | 'folder'

/**
 * The two live `<input type="file">` elements, or null before the layout has
 * mounted them. A module-scope handle rather than a prop or a context value
 * because the thing that opens a picker is a menu item three components away
 * from the input, and threading a ref through the rail to reach a hidden input
 * in the layout would be the same coupling wearing a disguise.
 */
const inputs: Record<PickerKind, HTMLInputElement | null> = { files: null, folder: null }

/**
 * Open the OS file chooser. Must be called synchronously from a real user
 * gesture — an `await` first spends the transient activation and the browser
 * silently ignores the click.
 */
export function openPicker(kind: PickerKind): void {
  inputs[kind]?.click()
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

/**
 * Walk a tree of dropped or picked items into `target`, then re-read `target`.
 *
 * Folders are created by `ingest` through a plain fetch inside the engine's
 * traverse module, which knows nothing about this cache. Without re-reading the
 * folder afterwards, a dropped tree is invisible until a manual reload — and an
 * EMPTY dropped folder, which enqueues no upload at all, is invisible for good
 * (the completion bridge only fires for files, and it invalidates the file's
 * own parent, which for a nested drop is the new subfolder).
 *
 * `target` is a parameter rather than something read here on the way out. The
 * walk takes as long as the tree takes, and a person who starts a folder upload
 * and then goes somewhere else must not have the folder they are now looking at
 * re-read instead of the one their files are landing in.
 */
export function useIngest(): (items: Parameters<typeof ingest>[0], target: string) => Promise<void> {
  const client = useQueryClient()
  return async (items, target) => {
    try {
      await ingest(items, target, sink)
    } finally {
      void client.invalidateQueries({ queryKey: childrenKey(target) })
    }
  }
}

/**
 * The file pickers, mounted once by the layout.
 *
 * They are two elements rather than one because `webkitdirectory` is a property
 * of the input, not of the click: an input that can pick a folder cannot pick
 * loose files. Automation drives the folder one directly — the day-0 spike
 * measured that CDP-synthesized drops carry no directory entry at all — which
 * is why both keep an accessible name despite never being visible.
 */
export function UploadPickers() {
  const folderId = useCurrentFolder()
  const ingestInto = useIngest()

  // The picker's `change` fires an unbounded time after the click — a person can
  // sit in the OS chooser for a minute — but it fires on whatever folder was
  // current when they opened it, which is the one they meant.
  const current = useRef(folderId)
  current.current = folderId

  return (
    <>
      <input
        ref={(el) => {
          inputs.files = el
          return () => {
            inputs.files = null
          }
        }}
        type="file"
        multiple
        aria-label="Upload files"
        className="hidden"
        onChange={(e) => {
          const target = current.current
          const files = e.target.files
          if (files) for (const file of files) uploadActions.enqueue(file, target)
          e.target.value = ''
        }}
      />
      <input
        ref={(el) => {
          inputs.folder = el
          return () => {
            inputs.folder = null
          }
        }}
        type="file"
        aria-label="Upload folder"
        className="hidden"
        // Not in React's attribute types; the DOM property is what Chromium reads.
        {...{ webkitdirectory: '' }}
        onChange={(e) => {
          const target = current.current
          const files = e.target.files
          if (files) void ingestInto(walkFileList(files), target)
          e.target.value = ''
        }}
      />
    </>
  )
}
