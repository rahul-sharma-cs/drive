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
  FileFetchError,
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
  // `run` claims the rejection of both the dialog and the walk before it yields,
  // so neither a dismissed dialog nor a cancelled walk is ever unhandled.
  void run(roots, name, handle, controller, deps)
}

/**
 * The save dialog, opened inside the click.
 *
 * A browser that refuses the call refuses it *synchronously* — an exception,
 * not a rejected promise — and an exception here would escape the click handler
 * having already put the dock on screen with nothing behind it. One shape comes
 * out of this function: a promise, or nothing at all because there is no picker.
 */
function openSaveDialog(name: string): Promise<FileSystemFileHandle> | undefined {
  if (!canStreamToDisk()) return undefined
  try {
    return window.showSaveFilePicker?.({
      suggestedName: name,
      types: [{ description: 'Zip archive', accept: { 'application/zip': ['.zip'] } }],
    })
  } catch (err) {
    return Promise.reject(err)
  }
}

async function run(
  roots: readonly DriveNode[],
  name: string,
  handle: Promise<FileSystemFileHandle> | undefined,
  ac: AbortController,
  deps: ZipDeps,
): Promise<void> {
  // Started in the same tick as the dialog rather than after it: the dialog is
  // a person choosing a folder — seconds, sometimes a minute — and the listing
  // needs nothing from their answer. Both run under `ac`, so a dismissed dialog
  // stops the walk it overlapped with.
  const walking = collectSubtree(roots, ac.signal, deps)
  // Claimed now rather than only at the `await` below, which sits behind the
  // dialog: a walk that fails or is cancelled while the dialog is still open
  // would otherwise be an unhandled rejection. The promise stays rejected, so
  // the `await` still sees it.
  void walking.catch(() => {})

  try {
    const file = handle === undefined ? null : await saveTarget(handle, ac)
    const entries = await walking
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
    if (!isAbort(err)) toast.error(failureMessage(err, name))
  } finally {
    controller = null
    publish(null)
  }
}

/**
 * The file the archive is written to, or an abort.
 *
 * Dismissing the dialog is an answer: it cancels the walk that has been running
 * behind it and says nothing else. Anything else is the browser declining to
 * open the dialog at all, and what it says about that ("Failed to execute
 * 'showSaveFilePicker' on 'Window': Must be handling a user gesture…") is a
 * console string, not a sentence to hand to whoever clicked Download.
 */
async function saveTarget(
  handle: Promise<FileSystemFileHandle>,
  ac: AbortController,
): Promise<FileSystemFileHandle> {
  try {
    return await handle
  } catch (err) {
    ac.abort(err)
    if (isAbort(err)) throw err
    throw new Error('Couldn’t open the save dialog — try again from a click')
  }
}

/**
 * A failure names the archive as well as the file that caused it: on the disk
 * path the browser created that file the moment the dialog was answered, and
 * where it cannot be deleted afterwards an empty one is left behind. The person
 * has to be able to tell which file on their disk is the dud.
 */
function failureMessage(err: unknown, name: string): string {
  if (err instanceof FileFetchError) {
    return `Couldn’t download “${err.fileName}” — “${name}” was not saved`
  }
  return (err as Error)?.message ?? 'The archive could not be built'
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
