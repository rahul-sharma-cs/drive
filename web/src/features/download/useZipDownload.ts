import { useSyncExternalStore } from 'react'
import { toast } from 'sonner'

import { downloadHref, type DriveNode } from '../../lib/api'
import {
  archiveBytes,
  archiveName,
  blobSink,
  canStreamToDisk,
  collectSubtree,
  diskSink,
  isAbort,
  liveDeps,
  MEMORY_LIMIT,
  writeArchive,
  type ZipDeps,
  type ZipEntry,
} from './zip'

/**
 * The one archive the app will build at a time, and the dock's view of it.
 *
 * A module-scope singleton rather than component state, for the same reason the
 * upload engine is one: the thing reporting on the work (`ZipDock`, in the app
 * layout) is nowhere near the thing that starts it (a row menu, a command
 * band), and navigating away mid-archive must not cancel it.
 */

export interface ZipJob {
  /** The archive's file name — what the dock is titled after. */
  name: string
  /** The entry being fetched right now, or '' before the first one. */
  current: string
  written: number
  /** Exact final size, known once the walk is done. 0 while walking. */
  total: number
}

let job: ZipJob | null = null
let controller: AbortController | null = null
const listeners = new Set<() => void>()

function publish(next: ZipJob | null): void {
  job = next
  for (const listener of listeners) listener()
}

const subscribe = (listener: () => void) => {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** The archive in progress, or null. Re-renders whatever reads it. */
export const useZipJob = (): ZipJob | null =>
  useSyncExternalStore(
    subscribe,
    () => job,
    () => null,
  )

/** The dock's button. Stops the walk, the fetches and the writer together. */
export function cancelZipDownload(): void {
  controller?.abort(new DOMException('The archive was cancelled.', 'AbortError'))
}

/**
 * Starts an archive of `roots`.
 *
 * **Synchronous up to and including `showSaveFilePicker()`, and it has to stay
 * that way.** The save dialog spends the click's transient user activation, and
 * the activation is gone long before the paginated walk below finishes — a
 * picker called after the first `await` throws `SecurityError` on a folder big
 * enough to be worth zipping, which is to say exactly when it matters.
 */
export function startZipDownload(
  roots: readonly DriveNode[],
  { deps = liveDeps, now = new Date() }: { deps?: ZipDeps; now?: Date } = {},
): void {
  if (controller !== null) {
    toast.error('One archive at a time')
    return
  }

  const name = archiveName(roots, now)
  const handle = canStreamToDisk()
    ? window.showSaveFilePicker?.({
        suggestedName: name,
        types: [{ description: 'Zip archive', accept: { 'application/zip': ['.zip'] } }],
      })
    : undefined

  controller = new AbortController()
  publish({ name, current: '', written: 0, total: 0 })
  // `run` awaits `handle` before it yields, so a cancelled save dialog is never
  // an unhandled rejection.
  void run(roots, name, handle, controller, deps)
}

async function run(
  roots: readonly DriveNode[],
  name: string,
  handle: Promise<FileSystemFileHandle> | undefined,
  ac: AbortController,
  deps: ZipDeps,
): Promise<void> {
  try {
    const file = handle === undefined ? null : await handle
    const entries = await collectSubtree(roots, ac.signal, deps)
    const total = archiveBytes(entries)

    // No File System Access: the whole archive would have to be assembled in
    // memory first. Past a point that is not a slow download, it is a dead tab —
    // so the offer changes rather than the size limit being ignored.
    if (file === null && total > MEMORY_LIMIT) {
      offerIndividually(entries)
      return
    }

    publish({ name, current: '', written: 0, total })
    const sink = file === null ? blobSink(name) : await diskSink(file)
    await writeArchive(entries, sink, ac.signal, deps, ({ written, current }) =>
      publish({ name, current, written, total }),
    )
  } catch (err) {
    // A cancel is an answer, not a failure — the person already knows.
    if (!isAbort(err)) toast.error((err as Error)?.message ?? 'The archive could not be built')
  } finally {
    controller = null
    publish(null)
  }
}

/**
 * The degraded path: one download per file, in order. Accepted as the answer
 * for large archives outside Chromium — the folder structure is lost, which is
 * why it is offered rather than done silently.
 */
function offerIndividually(entries: readonly ZipEntry[]): void {
  const ids = entries.flatMap((entry) => (entry.id === undefined ? [] : [entry.id]))
  toast.error(`That is too large for this browser to zip (${ids.length} files)`, {
    duration: 15_000,
    action: { label: 'Download files individually', onClick: () => void downloadEach(ids) },
  })
}

async function downloadEach(ids: readonly string[]): Promise<void> {
  for (const id of ids) {
    const anchor = document.createElement('a')
    anchor.href = downloadHref(id)
    anchor.target = '_blank'
    anchor.rel = 'noopener'
    anchor.click()
    // Staggered: a browser handed a hundred navigations in one tick drops most
    // of them, and the ones it keeps it treats as a popup storm.
    await new Promise((resolve) => setTimeout(resolve, 300))
  }
}

/** Tests only: the singleton outlives a test file otherwise. */
export function resetZipDownload(): void {
  controller = null
  job = null
}
