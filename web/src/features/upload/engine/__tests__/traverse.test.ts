import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  collectDropEntries,
  createFolderViaApi,
  ingest,
  walkEntries,
  walkFileList,
  type DropItem,
  type TraverseSink,
} from '../traverse'

// ---------------------------------------------------------------------------
// Fakes. No browser, no DOM: hand-built objects shaped exactly like Chromium's
// FileSystemEntry API, including the 100-entry readEntries cap.
// ---------------------------------------------------------------------------

/** Chromium hands back at most 100 entries per readEntries() call. */
const CHROMIUM_BATCH = 100

/** One logical tree node. `file` present => leaf; otherwise a directory. */
type Spec = { name: string; file?: File; children?: Spec[] }

function dir(name: string, children: Spec[]): Spec {
  return { name, children }
}

function file(name: string, body = 'x'): Spec {
  return { name, file: new File([body], name, { type: 'text/plain' }) }
}

type ReaderStats = { calls: number }

function fakeFileEntry(spec: Spec): FileSystemEntry {
  return {
    isFile: true,
    isDirectory: false,
    name: spec.name,
    file: (success: (f: File) => void) => success(spec.file as File),
  } as unknown as FileSystemEntry
}

function fakeDirEntry(spec: Spec, stats: ReaderStats): FileSystemEntry {
  const children = (spec.children ?? []).map((child) => fakeEntry(child, stats))
  return {
    isFile: false,
    isDirectory: true,
    name: spec.name,
    createReader: () => {
      // A fresh reader per call, but ONE cursor per reader: the walk must keep
      // calling the same reader until it returns an empty batch.
      let offset = 0
      return {
        readEntries: (success: (e: FileSystemEntry[]) => void) => {
          stats.calls++
          const batch = children.slice(offset, offset + CHROMIUM_BATCH)
          offset += batch.length
          success(batch)
        },
      }
    },
  } as unknown as FileSystemEntry
}

function fakeEntry(spec: Spec, stats: ReaderStats): FileSystemEntry {
  return spec.file ? fakeFileEntry(spec) : fakeDirEntry(spec, stats)
}

function dropEntries(specs: Spec[], stats: ReaderStats = { calls: 0 }): FileSystemEntry[] {
  return specs.map((spec) => fakeEntry(spec, stats))
}

/**
 * The webkitdirectory half of the same logical tree: a FLAT File list in
 * depth-first order, each File stamped with its webkitRelativePath. The File
 * objects are the SAME instances the entry fakes hand out, so a shared-core
 * assertion can compare them by identity.
 */
function pickerFiles(specs: Spec[]): File[] {
  const out: File[] = []
  const walk = (list: Spec[], prefix: string[]) => {
    for (const spec of list) {
      if (spec.file) {
        Object.defineProperty(spec.file, 'webkitRelativePath', {
          value: [...prefix, spec.name].join('/'),
          configurable: true,
        })
        out.push(spec.file)
        continue
      }
      walk(spec.children ?? [], [...prefix, spec.name])
    }
  }
  walk(specs, [])
  return out
}

async function collect(items: AsyncIterable<DropItem> | Iterable<DropItem>): Promise<DropItem[]> {
  const out: DropItem[] = []
  for await (const item of items) out.push(item)
  return out
}

/** Structural projection — File instances have no own enumerable props, so a
 *  bare toEqual on two DropItem lists would compare files vacuously. */
function shape(items: DropItem[]) {
  return items.map((item) =>
    item.kind === 'file'
      ? { kind: item.kind, path: item.path, name: item.file.name, size: item.file.size }
      : { kind: item.kind, path: item.path },
  )
}

function recordingSink(nextId = (n: number) => `id-${n}`) {
  let seq = 0
  const creates: Array<[string, string]> = []
  const enqueued: Array<[string, string]> = []
  const sink: TraverseSink = {
    createFolder: async (parentId, name) => {
      creates.push([parentId, name])
      return nextId(++seq)
    },
    enqueueFile: (parentId, f) => {
      enqueued.push([parentId, f.name])
    },
  }
  return { sink, creates, enqueued }
}

// ---------------------------------------------------------------------------

describe('collectDropEntries', () => {
  it('collects every entry synchronously, before the items list is invalidated', () => {
    const stats = { calls: 0 }
    const entries = dropEntries([dir('tree', [file('a.txt')]), file('loose.txt')], stats)
    let live = true
    const items = [
      { kind: 'string', webkitGetAsEntry: () => null },
      { kind: 'file', webkitGetAsEntry: () => (live ? entries[0] : null) },
      { kind: 'file', webkitGetAsEntry: () => (live ? entries[1] : null) },
    ]
    const dataTransfer = { items } as unknown as DataTransfer

    const collected = collectDropEntries(dataTransfer)

    // Synchronous by construction: an array, not a promise.
    expect(Array.isArray(collected)).toBe(true)
    expect(collected).toEqual([entries[0], entries[1]])

    // Now let the handler "yield": Chromium invalidates the items here. The
    // already-collected entries stay usable, which is the whole contract.
    live = false
    expect(collectDropEntries(dataTransfer)).toEqual([])
    expect(collected).toHaveLength(2)
  })

  it('skips non-file items and null entries', () => {
    const dataTransfer = {
      items: [
        { kind: 'string', webkitGetAsEntry: () => null },
        { kind: 'file', webkitGetAsEntry: () => null },
      ],
    } as unknown as DataTransfer
    expect(collectDropEntries(dataTransfer)).toEqual([])
  })
})

describe('walkEntries', () => {
  it('loops readEntries until an empty batch — 250 entries, not the first 100', async () => {
    const children = Array.from({ length: 250 }, (_, i) => file(`f${String(i).padStart(3, '0')}.txt`))
    const stats = { calls: 0 }
    const entries = dropEntries([dir('big', children)], stats)

    const items = await collect(walkEntries(entries))
    const files = items.filter((i) => i.kind === 'file')

    expect(files).toHaveLength(250)
    expect(files.map((i) => (i.kind === 'file' ? i.file.name : ''))).toEqual(
      children.map((c) => c.name),
    )
    // 100 + 100 + 50 + the empty batch that ends the loop.
    expect(stats.calls).toBe(4)
  })

  it('walks a 3-level tree depth-first', async () => {
    const entries = dropEntries([
      dir('tree', [
        file('root.txt'),
        dir('a', [file('a1.txt'), dir('deep', [file('d1.txt')])]),
        dir('b', [file('b1.txt')]),
      ]),
    ])

    expect(shape(await collect(walkEntries(entries)))).toEqual([
      { kind: 'dir', path: ['tree'] },
      { kind: 'file', path: ['tree'], name: 'root.txt', size: 1 },
      { kind: 'dir', path: ['tree', 'a'] },
      { kind: 'file', path: ['tree', 'a'], name: 'a1.txt', size: 1 },
      { kind: 'dir', path: ['tree', 'a', 'deep'] },
      { kind: 'file', path: ['tree', 'a', 'deep'], name: 'd1.txt', size: 1 },
      { kind: 'dir', path: ['tree', 'b'] },
      { kind: 'file', path: ['tree', 'b'], name: 'b1.txt', size: 1 },
    ])
  })

  it('emits empty directories so they are still created', async () => {
    const entries = dropEntries([dir('tree', [dir('empty', []), file('x.txt')])])

    expect(shape(await collect(walkEntries(entries)))).toEqual([
      { kind: 'dir', path: ['tree'] },
      { kind: 'dir', path: ['tree', 'empty'] },
      { kind: 'file', path: ['tree'], name: 'x.txt', size: 1 },
    ])
  })

  it('handles a file dropped at the root alongside directories', async () => {
    const entries = dropEntries([file('loose.txt'), dir('tree', [file('inner.txt')])])

    expect(shape(await collect(walkEntries(entries)))).toEqual([
      { kind: 'file', path: [], name: 'loose.txt', size: 1 },
      { kind: 'dir', path: ['tree'] },
      { kind: 'file', path: ['tree'], name: 'inner.txt', size: 1 },
    ])
  })
})

describe('the two ingress paths normalize to one iterator', () => {
  it('webkitdirectory File[] yields exactly what the entry walk yields', async () => {
    const tree = [
      dir('tree', [
        file('root.txt'),
        dir('a', [file('a1.txt'), file('a2.txt'), dir('deep', [file('d1.txt')])]),
        dir('b', [file('b1.txt')]),
      ]),
    ]

    const fromDrop = await collect(walkEntries(dropEntries(tree)))
    const fromPicker = await collect(walkFileList(pickerFiles(tree)))

    // Structural equality...
    expect(shape(fromPicker)).toEqual(shape(fromDrop))
    // ...and the File objects themselves are the same ones, in the same order.
    const dropFiles = fromDrop.flatMap((i) => (i.kind === 'file' ? [i.file] : []))
    const pickerFilesOut = fromPicker.flatMap((i) => (i.kind === 'file' ? [i.file] : []))
    expect(pickerFilesOut).toHaveLength(dropFiles.length)
    pickerFilesOut.forEach((f, i) => expect(f).toBe(dropFiles[i]))

    // And the shared core produces identical API calls from either source.
    const a = recordingSink()
    await ingest(walkEntries(dropEntries(tree)), 'root', a.sink)
    const b = recordingSink()
    await ingest(walkFileList(pickerFiles(tree)), 'root', b.sink)
    expect(b.creates).toEqual(a.creates)
    expect(b.enqueued).toEqual(a.enqueued)
  })

  it('a bare filename in the picker list lands at the drop root', async () => {
    const solo = new File(['x'], 'solo.txt')
    expect(shape(await collect(walkFileList([solo])))).toEqual([
      { kind: 'file', path: [], name: 'solo.txt', size: 1 },
    ])
  })
})

describe('ingest', () => {
  it('creates each folder exactly once and enqueues files against it', async () => {
    const children = Array.from({ length: 50 }, (_, i) => file(`f${String(i).padStart(2, '0')}.txt`))
    const { sink, creates, enqueued } = recordingSink()

    await ingest(walkEntries(dropEntries([dir('big', children)])), 'root', sink)

    expect(creates).toEqual([['root', 'big']])
    expect(enqueued).toHaveLength(50)
    expect(new Set(enqueued.map(([parentId]) => parentId))).toEqual(new Set(['id-1']))
  })

  it('memoizes by path — one create per unique folder, deepest parents resolved', async () => {
    const { sink, creates, enqueued } = recordingSink()

    await ingest(
      walkEntries(
        dropEntries([
          dir('tree', [
            file('root.txt'),
            dir('a', [file('a1.txt'), file('a2.txt'), dir('deep', [file('d1.txt')])]),
            dir('b', [file('b1.txt')]),
          ]),
        ]),
      ),
      'root',
      sink,
    )

    expect(creates).toEqual([
      ['root', 'tree'],
      ['id-1', 'a'],
      ['id-2', 'deep'],
      ['id-1', 'b'],
    ])
    expect(enqueued).toEqual([
      ['id-1', 'root.txt'],
      ['id-2', 'a1.txt'],
      ['id-2', 'a2.txt'],
      ['id-3', 'd1.txt'],
      ['id-4', 'b1.txt'],
    ])
  })

  it('enqueues a root-level file against the drop target itself', async () => {
    const { sink, creates, enqueued } = recordingSink()

    await ingest(walkEntries(dropEntries([file('loose.txt'), dir('tree', [file('in.txt')])])), 'root', sink)

    expect(creates).toEqual([['root', 'tree']])
    expect(enqueued).toEqual([
      ['root', 'loose.txt'],
      ['id-1', 'in.txt'],
    ])
  })

  it('one failed folder create never discards the rest of the drop', async () => {
    // PLAN §Conflict rules: bulk drops proceed without per-item prompts. A 5xx,
    // a name the server's filename hygiene rejects, or a collision with an
    // existing FILE used to reject `ingest` outright — in a 150-file drop that
    // means items 4..150 are never enqueued and nothing is ever reported.
    const tree = [
      dir('tree', [
        file('root.txt'),
        dir('a', [file('a1.txt'), dir('deep', [file('d1.txt')])]),
        dir('b', [file('b1.txt')]),
      ]),
    ]
    const creates: string[] = []
    const enqueued: Array<[string, string]> = []
    const errors: string[] = []
    let seq = 0
    const sink: TraverseSink = {
      createFolder: async (_parentId, name) => {
        creates.push(name)
        if (name === 'a') throw new Error('POST /folders failed: 500')
        return `id-${++seq}`
      },
      enqueueFile: (parentId, f) => {
        enqueued.push([parentId, f.name])
      },
      onError: (item) => {
        errors.push(item.kind === 'file' ? item.file.name : item.path.join('/'))
      },
    }

    await expect(ingest(walkEntries(dropEntries(tree)), 'root', sink)).resolves.toBeUndefined()

    // Everything outside the broken subtree still lands.
    expect(enqueued).toEqual([
      ['id-1', 'root.txt'],
      ['id-2', 'b1.txt'],
    ])
    // The failed path is memoized: `deep` is never even attempted, and `a` is
    // tried once, not once per child.
    expect(creates).toEqual(['tree', 'a', 'b'])
    // Every discarded item is reported, so 4b can show per-item error rows.
    expect(errors).toEqual(['tree/a', 'a1.txt', 'tree/a/deep', 'd1.txt'])
  })

  it('survives a failing folder even with no onError handler', async () => {
    const sink: TraverseSink = {
      createFolder: async () => {
        throw new Error('boom')
      },
      enqueueFile: () => undefined,
    }
    await expect(
      ingest(walkEntries(dropEntries([dir('tree', [file('x.txt')])])), 'root', sink),
    ).resolves.toBeUndefined()
  })

  it('creates an empty folder even though it enqueues nothing', async () => {
    const { sink, creates, enqueued } = recordingSink()

    await ingest(walkEntries(dropEntries([dir('tree', [dir('empty', [])])])), 'root', sink)

    expect(creates).toEqual([
      ['root', 'tree'],
      ['id-1', 'empty'],
    ])
    expect(enqueued).toEqual([])
  })
})

describe('createFolderViaApi', () => {
  afterEach(() => vi.unstubAllGlobals())

  function stubFetch(status: number, body: unknown) {
    const fetchMock = vi.fn(async () => ({
      ok: status < 400,
      status,
      json: async () => body,
    }))
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('posts conflict_policy=reuse with the client header', async () => {
    const fetchMock = stubFetch(201, { id: 'new-folder', existing: false })

    await expect(createFolderViaApi('parent-1', 'photos')).resolves.toBe('new-folder')

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe('/api/folders')
    expect(init.method).toBe('POST')
    expect(init.headers).toMatchObject({ 'X-Drive-Client': 'web' })
    expect(JSON.parse(init.body as string)).toEqual({
      parent_id: 'parent-1',
      name: 'photos',
      conflict_policy: 'reuse',
    })
  })

  it('reuses the existing folder on collision (200 existing:true)', async () => {
    stubFetch(200, { id: 'already-there', existing: true })
    await expect(createFolderViaApi('parent-1', 'photos')).resolves.toBe('already-there')
  })

  it('throws on a non-ok response', async () => {
    stubFetch(404, { code: 'not_found' })
    await expect(createFolderViaApi('gone', 'photos')).rejects.toThrow(/failed: 404/)
  })
})
