/**
 * `IdbStorage` against a real IndexedDB implementation (fake-indexeddb), plus
 * the two failure modes a round-trip test can never reach.
 *
 * This is the class that implements PLAN's "records keyed by upload_id, looked
 * up via a (fingerprint, parent_id) index" requirement. A typo in the keyPath,
 * the index key path, or the upgrade branch ships green everywhere else and
 * only shows up as "resume silently forgot the session" in a browser.
 *
 * A fresh `IDBFactory` per test — never `fake-indexeddb/auto` — keeps the
 * databases isolated and leaves no global behind.
 */

import { IDBFactory } from 'fake-indexeddb'
import { describe, expect, it } from 'vitest'
import { FP_INDEX, IdbStorage, STORE, type UploadRecord } from '../idb'

const record = (over: Partial<UploadRecord> = {}): UploadRecord => ({
  upload_id: 'up-1',
  fingerprint: 'fp-1',
  parent_id: 'parent-1',
  file_name: 'a.bin',
  file_size: 3000,
  part_size: 1000,
  parts_total: 3,
  confirmed_parts: [1, 2],
  status: 'active',
  session_expires_at: '2099-01-01T00:00:00Z',
  updated_at: 42,
  ...over,
})

const fresh = (): IdbStorage => {
  const factory = new IDBFactory()
  return new IdbStorage(() => factory)
}

describe('IdbStorage against a real IndexedDB', () => {
  it('round-trips a record: put → find by (fingerprint, parent_id) → remove', async () => {
    const storage = fresh()
    await storage.put(record())

    const found = await storage.find('fp-1', 'parent-1')
    expect(found).toEqual(record())

    await storage.remove('up-1')
    expect(await storage.find('fp-1', 'parent-1')).toBeNull()
  })

  it('keeps two records for the same file uploading into two folders', async () => {
    const storage = fresh()
    await storage.put(record({ upload_id: 'up-a', parent_id: 'folder-a' }))
    await storage.put(record({ upload_id: 'up-b', parent_id: 'folder-b' }))

    expect((await storage.find('fp-1', 'folder-a'))?.upload_id).toBe('up-a')
    expect((await storage.find('fp-1', 'folder-b'))?.upload_id).toBe('up-b')
    expect(await storage.find('fp-1', 'folder-c')).toBeNull()
  })

  it('overwrites in place — upload_id is the key, so progress never duplicates', async () => {
    const storage = fresh()
    await storage.put(record({ confirmed_parts: [1] }))
    await storage.put(record({ confirmed_parts: [1, 2, 3] }))

    expect((await storage.find('fp-1', 'parent-1'))?.confirmed_parts).toEqual([1, 2, 3])
  })

  it('creates the store with keyPath upload_id and the compound index', async () => {
    const factory = new IDBFactory()
    await new IdbStorage(() => factory).put(record())

    const db = await new Promise<IDBDatabase>((resolve, reject) => {
      const req = factory.open('drive-uploads')
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => reject(req.error)
    })
    const store = db.transaction(STORE, 'readonly').objectStore(STORE)
    expect(store.keyPath).toBe('upload_id')
    expect(store.index(FP_INDEX).keyPath).toEqual(['fingerprint', 'parent_id'])
    db.close()
  })

  it('finds nothing (rather than throwing) in a database with no records yet', async () => {
    expect(await fresh().find('fp-1', 'parent-1')).toBeNull()
  })
})

/* ------------------------------------------------------------------------- */
/* Failure modes a happy-path round trip can never reach. Hand-built stubs so  */
/* the test drives the exact event order the browser produces.                */
/* ------------------------------------------------------------------------- */

interface Stub {
  factory: IDBFactory
  fire: (event: 'success' | 'blocked' | 'error') => void
  tx: { oncomplete: (() => void) | null; onabort: (() => void) | null; onerror: (() => void) | null; error: unknown }
  req: { onsuccess: (() => void) | null; onerror: (() => void) | null; result: unknown; error: unknown }
}

function stubFactory(): Stub {
  const req = { onsuccess: null, onerror: null, result: undefined as unknown, error: undefined as unknown }
  const tx = {
    oncomplete: null as (() => void) | null,
    onabort: null as (() => void) | null,
    onerror: null as (() => void) | null,
    error: undefined as unknown,
    objectStore: () => ({ put: () => req, delete: () => req }),
  }
  const db = { transaction: () => tx }
  const openReq: Record<string, unknown> = { result: db, error: null }
  const factory = {
    open: () => openReq,
  } as unknown as IDBFactory
  return {
    factory,
    fire: (event) => {
      const handler = openReq[`on${event}`] as (() => void) | undefined
      handler?.()
    },
    tx,
    req,
  }
}

describe('IdbStorage durability', () => {
  it('resolves a write only when the TRANSACTION commits, not when the request succeeds', async () => {
    const stub = stubFactory()
    const storage = new IdbStorage(() => stub.factory)

    let settled: 'pending' | 'resolved' | 'rejected' = 'pending'
    const writing = storage.put(record()).then(
      () => (settled = 'resolved'),
      () => (settled = 'rejected'),
    )

    stub.fire('success')
    await Promise.resolve()
    stub.req.onsuccess?.()
    await Promise.resolve()
    await Promise.resolve()
    // req.onsuccess fires BEFORE the commit: reporting success here would call a
    // record durable that a commit-time abort then throws away.
    expect(settled).toBe('pending')

    stub.tx.oncomplete?.()
    await writing
    expect(settled).toBe('resolved')
  })

  it('rejects when the transaction aborts at commit time (QuotaExceededError)', async () => {
    const stub = stubFactory()
    const storage = new IdbStorage(() => stub.factory)

    const writing = storage.put(record())
    stub.fire('success')
    await Promise.resolve()
    stub.req.onsuccess?.()
    stub.tx.error = new Error('QuotaExceededError')
    stub.tx.onabort?.()

    await expect(writing).rejects.toThrow(/QuotaExceededError/)
  })

  it('rejects instead of hanging forever when another tab blocks the upgrade', async () => {
    const stub = stubFactory()
    const storage = new IdbStorage(() => stub.factory)

    const finding = storage.find('fp-1', 'parent-1')
    stub.fire('blocked')

    await expect(finding).rejects.toThrow(/blocked by another tab/)
  })
})
