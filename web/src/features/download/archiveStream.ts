import { downloadZip, predictLength } from 'client-zip'

import { FileFetchError, throwIfAborted, type ZipDeps, type ZipEntry, type ZipProgress, type ZipSink } from './zip'

/**
 * The half of the archive that needs client-zip, split off so the library is
 * not in the bundle a person waits for before they can see their files.
 *
 * Nothing reaches this module until a save dialog has been answered: the click
 * path in `useZipDownload` opens the picker synchronously — it must, or the
 * transient user activation is gone — starts the walk, and only then imports
 * what is in here, which by then is a download racing a person choosing a
 * folder rather than one blocking a first paint.
 */

/** Exact archive bytes: client-zip stores, so its own prediction is the answer. */
export const archiveBytes = (entries: readonly ZipEntry[]): number =>
  Number(
    predictLength(
      entries.map((entry) => (entry.id === undefined ? { name: entry.path } : { name: entry.path, size: entry.size })),
    ),
  )

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
