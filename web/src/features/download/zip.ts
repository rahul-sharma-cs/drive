import { downloadZip, predictLength } from 'client-zip'

import {
  getDownloadLink,
  listChildren,
  type BlobLink,
  type DriveNode,
  type Page,
} from '../../lib/api'

/**
 * Downloading more than one thing at a time: the archive is built here, in the
 * browser.
 *
 * There is no server-side archive endpoint and there is deliberately not going
 * to be one — bytes never pass through the Go app, so the only place a folder
 * can be turned into a zip is the one place the presigned links can be read:
 * this tab. The feature is therefore three pieces that this file keeps apart on
 * purpose, because each one fails differently:
 *
 *  - a **walk** (`collectSubtree`) — what is actually in there, paged;
 *  - a **stream** (`writeArchive`) — each file's bytes fetched from the store
 *    and fed to client-zip one at a time, so nothing is held in memory that is
 *    not on its way out;
 *  - a **sink** (`diskSink` / `blobSink`) — a real file on disk where the
 *    browser allows it, an in-memory Blob where it does not.
 *
 * Everything the browser supplies is injected (`ZipDeps`), the way the upload
 * engine does it, so the whole path is testable without a network or a disk.
 */

/* ------------------------------------------------------------------ shapes */

/**
 * One thing that goes into the archive.
 *
 * A `path` ending in `/` with no `id` is an empty folder — client-zip writes it
 * as a directory entry, which is what keeps an empty folder from vanishing out
 * of a zip of its parent.
 */
export interface ZipEntry {
  /** Where it sits inside the archive, folders included. Unique across the walk. */
  path: string
  /** The file whose bytes go here. Absent on a directory entry. */
  id?: string
  /** Uncompressed bytes. client-zip stores and never deflates, so this is real. */
  size: number
}

/**
 * The outside world, in one object.
 *
 * `fetchBytes` is a bare `fetch` on purpose and must stay one: `request()` in
 * `lib/api` adds `X-Drive-Client`, and a custom header on a cross-origin GET
 * turns it into a preflight that the store's enumerated CORS rule answers with
 * a 403. The link comes from the API; the bytes never do.
 */
export interface ZipDeps {
  listChildren: (id: string, cursor?: string) => Promise<Page<DriveNode>>
  getDownloadLink: (id: string) => Promise<BlobLink>
  fetchBytes: (url: string, signal: AbortSignal) => Promise<Response>
}

export const liveDeps: ZipDeps = {
  listChildren: (id, cursor) => listChildren(id, cursor),
  getDownloadLink,
  fetchBytes: (url, signal) => fetch(url, { signal }),
}

/** A file the store would not hand over. Carries the name the toast needs. */
export class FileFetchError extends Error {
  constructor(readonly fileName: string) {
    super(`Couldn’t download “${fileName}” — the archive was not saved`)
    this.name = 'FileFetchError'
  }
}

/**
 * Above this, a browser without the File System Access API would have to hold
 * the entire archive in memory before it could offer it — so that case is
 * turned into per-file downloads instead of an out-of-memory tab. Decimal, like
 * every other size this app quotes.
 */
export const MEMORY_LIMIT = 1_000_000_000

/** Chromium, essentially: the browsers that can stream an archive to disk. */
export const canStreamToDisk = (): boolean =>
  typeof window !== 'undefined' && typeof window.showSaveFilePicker === 'function'

/* -------------------------------------------------------------- the walk */

/**
 * Everything under `roots`, flattened into archive paths.
 *
 * Rejects rather than resolving short if `signal` fires: a truncated list would
 * become an archive that looks complete and silently is not, which is the one
 * outcome worse than no archive at all. The check that enforces that sits
 * immediately after each page lands — see `listAll`.
 *
 * Takes nodes rather than ids because every caller already holds them — the
 * band holds the selection, the row menu holds its row — and asking the server
 * again for names it just sent would be a round trip per root for nothing.
 */
export async function collectSubtree(
  roots: readonly DriveNode[],
  signal: AbortSignal,
  deps: ZipDeps = liveDeps,
): Promise<ZipEntry[]> {
  throwIfAborted(signal)
  const entries: ZipEntry[] = []
  await walk(roots, '', entries, signal, deps)
  return entries
}

async function walk(
  nodes: readonly DriveNode[],
  prefix: string,
  entries: ZipEntry[],
  signal: AbortSignal,
  deps: ZipDeps,
): Promise<void> {
  // One set per directory: names only have to be unique among their siblings,
  // and a name taken in one folder must not rename anything in another. Held
  // lower-cased — see `uniqueName`.
  const taken = new Set<string>()

  for (const node of nodes) {
    const path = prefix + uniqueName(node.name, taken)

    if (node.kind === 'file') {
      entries.push({ path, id: node.id, size: node.size ?? 0 })
      continue
    }

    const children = await listAll(node.id, signal, deps)
    if (children.length === 0) {
      entries.push({ path: `${path}/`, size: 0 })
      continue
    }
    await walk(children, `${path}/`, entries, signal, deps)
  }
}

/** Every page of one folder, or nothing at all if the walk is cancelled. */
async function listAll(id: string, signal: AbortSignal, deps: ZipDeps): Promise<DriveNode[]> {
  const items: DriveNode[] = []
  let cursor: string | undefined

  do {
    const page = await deps.listChildren(id, cursor)
    // Immediately after the page lands, and this is the only place it can go: a
    // request in flight is the only thing the walk ever waits on, so it is the
    // only moment a cancel can arrive. Miss it and the walk carries on asking
    // for the next page, then resolves — and a list that is short by whatever
    // was still in flight becomes an archive that looks complete and is not.
    throwIfAborted(signal)
    items.push(...page.items)
    cursor = page.next_cursor ?? undefined
  } while (cursor)

  return items
}

/**
 * Two things in one archive folder can share a name — a selection spanning two
 * folders, a search result, a folder holding a file of the same name as a
 * subfolder. A zip with two identical paths extracts as one file, so the second
 * is renamed. The suffix goes before the extension, where every file manager
 * puts it, so `notes.txt` becomes `notes (1).txt` and still opens as text.
 *
 * Collisions are judged case-insensitively, exactly as the server's
 * `NextFreeName` judges siblings: `Report.txt` and `report.txt` are two
 * distinct entries in a zip, but macOS and Windows extract them onto one
 * case-insensitive filesystem and the second silently replaces the first. Two
 * roots that differ only in case cannot be siblings on the server, but they
 * reach here together out of a search selection. The name that goes into the
 * archive keeps its own casing; only the bookkeeping is folded.
 *
 * Names cannot contain `/` or `\` — the server rejects both — so a name is
 * always exactly one path segment here.
 */
function uniqueName(name: string, taken: Set<string>): string {
  const dot = name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const ext = dot > 0 ? name.slice(dot) : ''

  let candidate = name
  for (let n = 1; taken.has(candidate.toLowerCase()); n++) candidate = `${stem} (${n})${ext}`
  taken.add(candidate.toLowerCase())
  return candidate
}

/* ----------------------------------------------------------- the archive */

/** Exact archive bytes: client-zip stores, so its own prediction is the answer. */
export const archiveBytes = (entries: readonly ZipEntry[]): number =>
  Number(
    predictLength(
      entries.map((entry) => (entry.id === undefined ? { name: entry.path } : { name: entry.path, size: entry.size })),
    ),
  )

/** Where the archive's bytes end up. Two implementations; one shape. */
export interface ZipSink {
  write(chunk: Uint8Array): Promise<void>
  close(): Promise<void>
  abort(reason: unknown): Promise<void>
}

/** Straight to the file the person picked. No size limit — nothing is buffered. */
export async function diskSink(handle: FileSystemFileHandle): Promise<ZipSink> {
  const writer = (await handle.createWritable()).getWriter()
  return {
    write: (chunk) => writer.write(chunk),
    close: () => writer.close(),
    abort: async (reason) => {
      await writer.abort(reason)
      // Aborting the writer throws away the bytes but not the file: the picker
      // created it the moment it was named, so a failed archive otherwise
      // leaves an empty file wearing the archive's name, which is precisely the
      // thing the abort exists to prevent. `remove()` is newer than the rest of
      // File System Access and missing from older Chromium, so it is
      // feature-detected — and its own failure changes nothing, because the
      // real error is already on its way to the toast.
      await handle.remove?.().catch(() => {})
    },
  }
}

/** Everywhere else: the whole archive in memory, then handed over as a file. */
export function blobSink(name: string): ZipSink {
  let chunks: Uint8Array[] = []
  return {
    write: async (chunk) => {
      chunks.push(chunk)
    },
    close: async () => {
      saveBlob(new Blob(chunks as BlobPart[], { type: 'application/zip' }), name)
      chunks = []
    },
    abort: async () => {
      chunks = []
    },
  }
}

function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = name
  anchor.click()
  // Not revoked in the same tick: Safari has been known to drop the download if
  // the URL stops resolving before it has read it.
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}

export interface ZipProgress {
  /** The entry being fetched right now — what the dock names. */
  current: string
  /** Archive bytes handed to the sink so far. */
  written: number
}

/**
 * How often progress is worth reporting.
 *
 * The stream hands over a chunk every few kilobytes, and a report is a React
 * render of the whole dock — tens of thousands of them across a large archive,
 * nearly all for a byte count that has not visibly moved. One report per 64 KB
 * or 100 ms keeps the bar smooth at any speed, and the file being fetched and
 * the last byte are always reported outright.
 */
const PROGRESS_BYTES = 64 * 1024
const PROGRESS_MS = 100

/**
 * Builds the archive and pumps it into `sink`, reporting as it goes.
 *
 * The pump is written out rather than left to `pipeTo` for two reasons: this is
 * the only place the byte count the dock shows can come from, and a failure has
 * to *abort* the sink rather than close it. A half-written file the browser has
 * already named is worse than no file — it looks like the archive.
 */
export async function writeArchive(
  entries: readonly ZipEntry[],
  sink: ZipSink,
  signal: AbortSignal,
  deps: ZipDeps,
  onProgress: (progress: ZipProgress) => void,
): Promise<void> {
  let written = 0
  let current = ''
  let reportedBytes = 0
  let reportedAt = Date.now()

  /** `always` for the two moments the dock must show exactly: a new file, and the last byte. */
  const report = (always = false): void => {
    const now = Date.now()
    if (!always && written - reportedBytes < PROGRESS_BYTES && now - reportedAt < PROGRESS_MS) return
    reportedBytes = written
    reportedAt = now
    onProgress({ written, current })
  }

  const body = downloadZip(
    zipInputs(entries, signal, deps, (path) => {
      current = path
      report(true)
    }),
  ).body
  if (body === null) throw new Error('the archive stream has no body')

  const reader = body.getReader()
  try {
    for (;;) {
      throwIfAborted(signal)
      const { done, value } = await reader.read()
      if (done) break
      await sink.write(value)
      written += value.byteLength
      report()
    }
    report(true)
    await sink.close()
  } catch (err) {
    await sink.abort(err).catch(() => {})
    await reader.cancel(err).catch(() => {})
    throw err
  }
}

/**
 * Lazily, one file at a time: the link is asked for and the bytes are fetched
 * only when client-zip is ready to write that entry, so a thousand-file archive
 * never has a thousand presigned URLs in flight or a thousand responses open.
 */
async function* zipInputs(
  entries: readonly ZipEntry[],
  signal: AbortSignal,
  deps: ZipDeps,
  onEntry: (path: string) => void,
) {
  for (const entry of entries) {
    throwIfAborted(signal)

    if (entry.id === undefined) {
      // No input and no size: client-zip reads that as a directory entry.
      yield { name: entry.path }
      continue
    }

    onEntry(entry.path)
    const link = await deps.getDownloadLink(entry.id)
    const res = await deps.fetchBytes(link.url, signal)
    if (!res.ok) throw new FileFetchError(baseName(entry.path))
    yield { name: entry.path, input: res }
  }
}

const baseName = (path: string): string => path.slice(path.lastIndexOf('/') + 1)

/* ------------------------------------------------------------------ names */

/**
 * One folder is downloaded under its own name; anything else is a set, and a
 * set has no name but the moment it was asked for.
 */
export function archiveName(roots: readonly DriveNode[], now: Date): string {
  if (roots.length === 1 && roots[0].kind === 'folder') return `${roots[0].name}.zip`
  const pad = (n: number) => String(n).padStart(2, '0')
  return `drive-${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(
    now.getMinutes(),
  )}.zip`
}

/* ---------------------------------------------------------------- aborting */

export const isAbort = (err: unknown): boolean => (err as { name?: string })?.name === 'AbortError'

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw new DOMException('The archive was cancelled.', 'AbortError')
}

declare global {
  interface Window {
    /** File System Access. Chromium only, and absent from TypeScript's DOM lib. */
    showSaveFilePicker?: (options?: {
      suggestedName?: string
      types?: { description?: string; accept: Record<string, string[]> }[]
    }) => Promise<FileSystemFileHandle>
  }

  interface FileSystemFileHandle {
    /** Deletes the file itself. Newer than the rest of File System Access, hence optional. */
    remove?: () => Promise<void>
  }
}
