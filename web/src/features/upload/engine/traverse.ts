/**
 * Folder-drop ingress — PLAN §Frontend "Folder drop traversal contract".
 *
 * Two ingress paths exist and they produce completely different raw shapes:
 *
 *   1. drag-drop      DataTransferItem -> FileSystemEntry tree (Chromium's
 *                     non-standard `webkitGetAsEntry`), walked asynchronously.
 *   2. folder picker  <input type="file" webkitdirectory> -> a FLAT File[]
 *                     where each File carries `webkitRelativePath`.
 *
 * Both normalize into ONE iterator of `DropItem`, and exactly one consumer --
 * `ingest()` -- creates folders and enqueues files. That is load-bearing, not
 * decorative: the day-0 spike (BRIEF build log, F2) measured that CDP-synthesized
 * folder drops produce no directory entry in this environment, so the Playwright
 * folder suite drives path 2. If the two paths did not share this core, the e2e
 * suite would be testing code the product does not use.
 *
 * Two hazards this module exists to make un-hittable:
 *   - DataTransferItem objects are invalidated the moment the drop handler
 *     yields to the event loop, so `collectDropEntries()` is SYNCHRONOUS and
 *     separate from the async walk.
 *   - Chromium's `readEntries()` returns at most 100 entries per call; reading
 *     one batch and stopping is the classic silent-truncation bug, so the
 *     directory reader loops until it gets an EMPTY batch.
 */

/**
 * One normalized ingress item.
 *
 * `path` is the folder path relative to the drop root, as segments. For a file
 * it is the file's PARENT folder ("" -> the drop target itself); for a dir it
 * is the folder's own path. Dropping `tree/` yields `['tree']` for that folder
 * and `['tree']` for a file directly inside it -- i.e. the dropped folder's own
 * name is part of the tree, matching what `webkitRelativePath` reports.
 *
 * The `dir` variant is what gives EMPTY folders a defined outcome: they are
 * created, not silently dropped. (A `webkitdirectory` File[] cannot express an
 * empty folder -- the browser omits them -- so that asymmetry is inherent to
 * the ingress path, not to this code.)
 */
export type DropItem =
  | { kind: 'file'; path: string[]; file: File }
  | { kind: 'dir'; path: string[] }

/**
 * SYNCHRONOUS collection of the drop's entries. Call this FIRST in the drop
 * handler, before any `await`: `dataTransfer.items` is invalidated as soon as
 * the handler returns or yields, and every entry read after that point is null.
 * The returned entries stay valid; only the item list dies.
 */
export function collectDropEntries(dataTransfer: DataTransfer): FileSystemEntry[] {
  const entries: FileSystemEntry[] = []
  const items = dataTransfer.items
  for (let i = 0; i < items.length; i++) {
    const item = items[i]
    if (item.kind !== 'file') continue
    const entry = item.webkitGetAsEntry()
    if (entry) entries.push(entry)
  }
  return entries
}

/** Ingress 1: walk dropped FileSystemEntry trees depth-first. */
export async function* walkEntries(entries: FileSystemEntry[]): AsyncGenerator<DropItem> {
  for (const entry of entries) {
    yield* walkEntry(entry, [])
  }
}

async function* walkEntry(entry: FileSystemEntry, parentPath: string[]): AsyncGenerator<DropItem> {
  if (entry.isFile) {
    yield { kind: 'file', path: parentPath, file: await readFile(entry as FileSystemFileEntry) }
    return
  }
  if (!entry.isDirectory) return
  const path = [...parentPath, entry.name]
  yield { kind: 'dir', path }
  for (const child of await readAllEntries(entry as FileSystemDirectoryEntry)) {
    yield* walkEntry(child, path)
  }
}

/**
 * Read a directory to exhaustion. `readEntries` yields at most 100 entries per
 * call and signals "done" only with an empty batch -- one call is never enough
 * for a directory of 101+ children.
 */
async function readAllEntries(dir: FileSystemDirectoryEntry): Promise<FileSystemEntry[]> {
  const reader = dir.createReader()
  const all: FileSystemEntry[] = []
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => {
      reader.readEntries(resolve, reject)
    })
    if (batch.length === 0) return all
    all.push(...batch)
  }
}

function readFile(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => {
    entry.file(resolve, reject)
  })
}

/**
 * Ingress 2: the `webkitdirectory` picker's flat File list. Each File's
 * `webkitRelativePath` ("tree/a/b.txt") carries the tree; a bare name means a
 * file at the drop root. Emits a `dir` item the first time each folder prefix
 * appears, so the item stream is identical to `walkEntries()` for the same
 * logical tree in the same depth-first order.
 */
export function* walkFileList(files: ArrayLike<File>): Generator<DropItem> {
  const seen = new Set<string>()
  for (let i = 0; i < files.length; i++) {
    const file = files[i]
    const relative = file.webkitRelativePath || file.name
    const path = relative.split('/').slice(0, -1)
    for (let depth = 1; depth <= path.length; depth++) {
      const prefix = path.slice(0, depth)
      const key = prefix.join('/')
      if (seen.has(key)) continue
      seen.add(key)
      yield { kind: 'dir', path: prefix }
    }
    yield { kind: 'file', path, file }
  }
}

/** Creates one folder and returns its id. See `createFolderViaApi`. */
export type FolderCreator = (parentId: string, name: string) => Promise<string>

export interface TraverseSink {
  createFolder: FolderCreator
  /** Hand one file to the upload engine, already bound to its real parent id. */
  enqueueFile: (parentId: string, file: File) => void
  /**
   * One item could not be placed (its folder — or an ancestor — failed to
   * create). The drop CONTINUES; 4b surfaces these as per-item error rows.
   */
  onError?: (item: DropItem, err: unknown) => void
}

/**
 * The single consumer of both ingress paths: create folders depth-first,
 * memoized by path so each folder is created exactly once no matter how many
 * children reference it, and enqueue every file against its resolved parent id.
 *
 * Sequential by construction -- a concurrent version would create the same
 * folder twice before the first create resolved.
 *
 * ONE failed folder create must never discard the rest of the drop: a 5xx, a
 * name the server's filename hygiene rejects, or a collision with an existing
 * FILE of that name would otherwise reject `ingest` at item 3 of 150, leaving
 * files 4..150 never enqueued and no per-item error anywhere (PLAN §Conflict
 * rules: bulk drops proceed without per-item prompts). So each item is isolated,
 * and a failed folder path is memoized -- its descendants are reported, never
 * retried once per child.
 */
export async function ingest(
  items: AsyncIterable<DropItem> | Iterable<DropItem>,
  rootId: string,
  sink: TraverseSink,
): Promise<void> {
  const ids = new Map<string, string>([['', rootId]])
  const failed = new Map<string, unknown>()

  const resolveParent = async (path: string[]): Promise<string> => {
    let key = ''
    let parentId = rootId
    for (const segment of path) {
      key = key === '' ? segment : `${key}/${segment}`
      const known = ids.get(key)
      if (known !== undefined) {
        parentId = known
        continue
      }
      if (failed.has(key)) throw failed.get(key)
      try {
        parentId = await sink.createFolder(parentId, segment)
      } catch (e) {
        failed.set(key, e)
        throw e
      }
      ids.set(key, parentId)
    }
    return parentId
  }

  for await (const item of items) {
    try {
      const parentId = await resolveParent(item.path)
      if (item.kind === 'file') sink.enqueueFile(parentId, item.file)
    } catch (e) {
      sink.onError?.(item, e)
    }
  }
}

/**
 * The real folder creator: `POST /api/folders` with `conflict_policy=reuse`,
 * which answers a collision with the EXISTING folder (`200 {..., existing:
 * true}`) instead of 409. That is what makes re-dropping a tree merge into the
 * folders already there rather than duplicating them.
 */
export async function createFolderViaApi(parentId: string, name: string): Promise<string> {
  const res = await fetch('/api/folders', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Drive-Client': 'web' },
    body: JSON.stringify({ parent_id: parentId, name, conflict_policy: 'reuse' }),
  })
  if (!res.ok) throw new Error(`create folder "${name}" failed: ${res.status}`)
  const body = (await res.json()) as { id: string }
  return body.id
}
