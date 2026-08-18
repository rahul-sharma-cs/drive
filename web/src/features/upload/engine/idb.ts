/**
 * Local upload records.
 *
 * Records are keyed by `upload_id` and
 * looked up through a `(fingerprint, parent_id)` index — matching the server's
 * uniqueness rule, so the same file uploading to two folders keeps two records.
 * The record is held in memory and the WHOLE record is written at most ~1 Hz:
 * per-progress-tick read-modify-write races wipe fields (a real bug in a
 * tested reference implementation).
 */

import type { Clock, SessionState } from './types'

export interface UploadRecord {
  upload_id: string
  fingerprint: string
  parent_id: string
  file_name: string
  file_size: number
  part_size: number
  parts_total: number
  confirmed_parts: number[]
  status: SessionState
  session_expires_at: string | null
  updated_at: number
}

export interface RecordStorage {
  put(record: UploadRecord): Promise<void>
  remove(uploadId: string): Promise<void>
  find(fingerprint: string, parentId: string): Promise<UploadRecord | null>
}

export const DB_NAME = 'drive-uploads'
export const STORE = 'uploads'
export const FP_INDEX = 'by_fingerprint_parent'

/** Thin IndexedDB adapter. All coalescing lives in RecordCache, not here. */
export class IdbStorage implements RecordStorage {
  private db: Promise<IDBDatabase> | null = null

  constructor(private readonly factory: () => IDBFactory = () => indexedDB) {}

  private open(): Promise<IDBDatabase> {
    if (!this.db) {
      this.db = new Promise<IDBDatabase>((resolve, reject) => {
        const req = this.factory().open(DB_NAME, 1)
        req.onupgradeneeded = () => {
          const db = req.result
          const store = db.objectStoreNames.contains(STORE)
            ? req.transaction!.objectStore(STORE)
            : db.createObjectStore(STORE, { keyPath: 'upload_id' })
          if (!store.indexNames.contains(FP_INDEX)) {
            store.createIndex(FP_INDEX, ['fingerprint', 'parent_id'], { unique: false })
          }
        }
        req.onsuccess = () => resolve(req.result)
        req.onerror = () => reject(req.error)
        // Another tab holding an older version blocks the upgrade forever;
        // without this every save()/find() would hang with no error at all.
        req.onblocked = () => reject(new Error(`${DB_NAME} is blocked by another tab`))
      })
    }
    return this.db
  }

  async put(record: UploadRecord): Promise<void> {
    const db = await this.open()
    await this.tx(db, 'readwrite', (store) => store.put(record))
  }

  async remove(uploadId: string): Promise<void> {
    const db = await this.open()
    await this.tx(db, 'readwrite', (store) => store.delete(uploadId))
  }

  async find(fingerprint: string, parentId: string): Promise<UploadRecord | null> {
    const db = await this.open()
    const got = await this.tx<UploadRecord | undefined>(db, 'readonly', (store) =>
      store.index(FP_INDEX).get([fingerprint, parentId]),
    )
    return got ?? null
  }

  /**
   * Resolves on `tx.oncomplete`, NOT on `req.onsuccess`: a request succeeds
   * before the transaction commits, so resolving there reports a write as
   * durable that a commit-time abort (QuotaExceededError is the common one)
   * then throws away — silently losing the resume record.
   */
  private tx<T>(
    db: IDBDatabase,
    mode: IDBTransactionMode,
    run: (store: IDBObjectStore) => IDBRequest,
  ): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const tx = db.transaction(STORE, mode)
      let result: T
      const req = run(tx.objectStore(STORE))
      req.onsuccess = () => {
        result = req.result as T
      }
      req.onerror = () => reject(req.error)
      tx.oncomplete = () => resolve(result)
      tx.onabort = () => reject(tx.error ?? new Error('upload record transaction aborted'))
      tx.onerror = () => reject(tx.error ?? new Error('upload record transaction failed'))
    })
  }
}

/**
 * In-memory authority over the records, flushing whole records to storage at
 * most once per `intervalMs` (leading write, then one trailing write).
 */
export class RecordCache {
  private readonly memory = new Map<string, UploadRecord>()
  private readonly dirty = new Set<string>()
  private lastWrite = -Infinity
  private timer: number | null = null
  private flushing: Promise<void> | null = null

  constructor(
    private readonly storage: RecordStorage,
    private readonly clock: Clock,
    private readonly intervalMs = 1000,
  ) {}

  get(uploadId: string): UploadRecord | undefined {
    return this.memory.get(uploadId)
  }

  /** Record the new state in memory; persist it on the ~1 Hz schedule. */
  save(record: UploadRecord): void {
    this.memory.set(record.upload_id, record)
    this.dirty.add(record.upload_id)
    if (this.timer !== null) return
    const since = this.clock.now() - this.lastWrite
    if (since >= this.intervalMs) {
      void this.write()
      return
    }
    this.timer = this.clock.setTimeout(() => {
      this.timer = null
      void this.write()
    }, this.intervalMs - since)
  }

  async find(fingerprint: string, parentId: string): Promise<UploadRecord | null> {
    for (const rec of this.memory.values()) {
      if (rec.fingerprint === fingerprint && rec.parent_id === parentId) return rec
    }
    return this.storage.find(fingerprint, parentId)
  }

  async remove(uploadId: string): Promise<void> {
    this.memory.delete(uploadId)
    this.dirty.delete(uploadId)
    await this.storage.remove(uploadId)
  }

  /** Write everything pending right now (used on terminal transitions). */
  async flush(): Promise<void> {
    if (this.timer !== null) {
      this.clock.clearTimeout(this.timer)
      this.timer = null
    }
    await this.write()
  }

  private write(): Promise<void> {
    if (this.flushing) return this.flushing
    const ids = [...this.dirty]
    this.dirty.clear()
    this.lastWrite = this.clock.now()
    this.flushing = (async () => {
      for (const id of ids) {
        const rec = this.memory.get(id)
        if (rec) await this.storage.put(rec)
      }
    })()
      .catch(() => undefined)
      .finally(() => {
        this.flushing = null
      })
    return this.flushing
  }
}
