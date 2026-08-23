import { useSyncExternalStore } from 'react'
import { toast } from 'sonner'

import { type DriveNode } from '../../lib/api'
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

/** One file out of an archive that will not be built, offered on its own. */
export interface OfferedFile {
  id: string
  /** Its place in the archive that was not built — two same-named files differ only here. */
  path: string
  size: number
}

/**
 * The degraded offer: everything the archive would have held, and what it would
 * have weighed. Held next to the job rather than inside it because it outlives
 * the job — the dock is gone by the time this is on screen.
 */
export interface ZipOffer {
  files: OfferedFile[]
  total: number
}

let job: ZipJob | null = null
let offer: ZipOffer | null = null
let controller: AbortController | null = null
const listeners = new Set<() => void>()

function announce(): void {
  for (const listener of listeners) listener()
}

function publish(next: ZipJob | null): void {
  job = next
  announce()
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

/** The per-file offer standing on screen, or null. */
export const useZipOffer = (): ZipOffer | null =>
  useSyncExternalStore(
    subscribe,
    () => offer,
    () => null,
  )

/** Closing the offer. Nothing is in flight behind it — it is a list of links. */
export function dismissZipOffer(): void {
  offer = null
  announce()
}

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
  const handle = openSaveDialog(name)

  controller = new AbortController()
  publish({ name, current: '', written: 0, total: 0 })
  // `run` awaits `handle` before it yields, so a cancelled save dialog is never
  // an unhandled rejection.
  void run(roots, name, handle, controller, deps)
}

/** The save dialog, opened inside the click. */
function openSaveDialog(name: string): Promise<FileSystemFileHandle> | undefined {
  if (!canStreamToDisk()) return undefined
  return window.showSaveFilePicker?.({
    suggestedName: name,
    types: [{ description: 'Zip archive', accept: { 'application/zip': ['.zip'] } }],
  })
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
      offerIndividually(entries, total)
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
 * The degraded path: the files, offered one at a time, as links.
 *
 * Not a toast with a button that downloads them in a loop — a browser grants
 * one navigation per click and treats the rest as a popup storm, so a loop
 * delivers the first file and silently drops the other ninety-nine. Every link
 * in the dialog is its own click, which is the one thing every browser on this
 * path honours. The folder structure is lost, which is why this is an offer on
 * screen rather than something done quietly.
 */
function offerIndividually(entries: readonly ZipEntry[], total: number): void {
  const files = entries.flatMap((entry) =>
    entry.id === undefined ? [] : [{ id: entry.id, path: entry.path, size: entry.size }],
  )
  offer = { files, total }
  announce()
}

/** Tests only: the singleton outlives a test file otherwise. */
export function resetZipDownload(): void {
  controller = null
  job = null
  offer = null
}
