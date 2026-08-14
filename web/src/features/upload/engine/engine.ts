/**
 * The upload engine singleton.
 *
 * PLAN §Client engine state machine: a module-scope singleton living OUTSIDE
 * the React tree, so navigating between routes never unmounts an upload. The
 * UI subscribes with `useSyncExternalStore(engine.subscribe, engine.getSnapshot)`
 * — `getSnapshot()` returns a cached, immutable object and only changes
 * identity when something actually changed.
 *
 * Nothing browser-global is touched until the first `enqueue()`: the module is
 * imported by vitest in a node environment.
 */

import { HttpUploadApi, xhrPutPart } from './api'
import { spawnMd5Worker, spawnSha256Worker } from './hash/hash'
import { IdbStorage, RecordCache, type RecordStorage } from './idb'
import { UploadMachine, type MachineDeps } from './machine'
import type {
  Clock,
  ConflictPolicy,
  EngineSnapshot,
  HashLike,
  LockManagerLike,
  PartPutter,
  UploadApi,
  UploadSnapshot,
} from './types'

export interface EngineEnv {
  api: UploadApi
  put: PartPutter
  /** Factories: hash workers/IDB/locks are created on first use, not on import. */
  md5: () => HashLike
  sha256: () => HashLike
  storage: () => RecordStorage
  locks: () => LockManagerLike
  clock: Clock
  random: () => number
  /** Registers a global listener; omitted in tests, which call notify* directly. */
  listen?: (type: 'online' | 'offline' | 'visibilitychange', fn: () => void) => void
  isVisible?: () => boolean
  concurrency?: number
  stallMs?: number
  pollMs?: number
  saveIntervalMs?: number
  /** Uploads running at once; the rest queue (a folder drop is 150 files). */
  maxActive?: number
}

let seq = 0

export class UploadEngine {
  private readonly env: EngineEnv
  private readonly machines: UploadMachine[] = []
  private readonly listeners = new Set<() => void>()
  private snap: EngineSnapshot = { items: [] }
  private dirty = true
  private booted = false
  private scheduling = false
  private deps: MachineDeps | null = null

  constructor(env: EngineEnv) {
    this.env = env
  }

  /* ---------------------------------------- useSyncExternalStore contract */

  readonly subscribe = (cb: () => void): (() => void) => {
    this.listeners.add(cb)
    return () => {
      this.listeners.delete(cb)
    }
  }

  readonly getSnapshot = (): EngineSnapshot => {
    if (this.dirty) {
      this.snap = { items: this.machines.map((m) => m.snapshot()) }
      this.dirty = false
    }
    return this.snap
  }

  /* --------------------------------------------------------------- API */

  /** Queues a file. Returns the client-side upload id used by every action. */
  enqueue(
    file: File,
    parentId: string,
    opts: { mime?: string; conflictPolicy?: ConflictPolicy } = {},
  ): string {
    const deps = this.boot()
    const id = `u${++seq}`
    this.machines.push(
      new UploadMachine({ id, file, parentId, mime: opts.mime, conflictPolicy: opts.conflictPolicy }, deps, () =>
        this.changed(),
      ),
    )
    this.changed()
    return id
  }

  pause(id: string): void {
    const machine = this.find(id)
    if (!machine) return
    machine.pause()
    // PLAN: pause also suspends both hash workers — but there is ONE worker
    // pair for the whole engine, so only the engine may suspend them. Pausing
    // above already let `schedule()` admit the next queued upload, so if any
    // machine is still active the workers must keep running; suspending them
    // here would hang every other upload in the manager with no error.
    if (this.deps && this.machines.every((m) => m.isIdle())) {
      this.deps.md5.suspend()
      this.deps.sha256.suspend()
    }
    this.changed()
  }

  /**
   * Hands a re-selected File to an existing row (PLAN §Resume: after a reload,
   * or after `error_file_changed`, "the user re-selects the file"). The row
   * keeps its id; the fingerprint is recomputed and decides resume vs fresh.
   */
  reselect(id: string, file: File): void {
    this.find(id)?.reselect(file)
    this.changed()
  }

  resume(id: string): void {
    this.find(id)?.resume()
    this.changed()
  }

  retry(id: string): void {
    this.find(id)?.retry()
    this.changed()
  }

  resolveConflict(id: string, policy: ConflictPolicy): void {
    this.find(id)?.resolveConflict(policy)
    this.changed()
  }

  async cancel(id: string): Promise<void> {
    const machine = this.find(id)
    if (!machine) return
    await machine.cancel()
    this.changed()
  }

  /** Drops finished rows from the manager list. */
  clearFinished(): void {
    for (let i = this.machines.length - 1; i >= 0; i--) {
      const s = this.machines[i].snapshot().state
      if (s === 'done' || s === 'canceled') this.machines.splice(i, 1)
    }
    this.changed()
  }

  get(id: string): UploadSnapshot | undefined {
    return this.getSnapshot().items.find((i) => i.id === id)
  }

  /* ------------------------------------------------------------- events */

  notifyOffline(): void {
    for (const m of this.machines) m.handleOffline()
    this.changed()
  }

  notifyOnline(): void {
    for (const m of this.machines) m.handleOnline()
    this.changed()
  }

  notifyVisible(): void {
    for (const m of this.machines) m.handleVisible()
    this.changed()
  }

  /* ---------------------------------------------------------- internals */

  private boot(): MachineDeps {
    if (this.deps) return this.deps
    this.deps = {
      api: this.env.api,
      put: this.env.put,
      md5: this.env.md5(),
      sha256: this.env.sha256(),
      cache: new RecordCache(this.env.storage(), this.env.clock, this.env.saveIntervalMs),
      clock: this.env.clock,
      random: this.env.random,
      locks: this.env.locks(),
      concurrency: this.env.concurrency,
      stallMs: this.env.stallMs,
      pollMs: this.env.pollMs,
    }
    if (!this.booted) {
      this.booted = true
      this.env.listen?.('offline', () => this.notifyOffline())
      this.env.listen?.('online', () => this.notifyOnline())
      this.env.listen?.('visibilitychange', () => {
        if (this.env.isVisible?.() ?? true) this.notifyVisible()
      })
    }
    return this.deps
  }

  private find(id: string): UploadMachine | undefined {
    return this.machines.find((m) => m.id === id)
  }

  private changed(): void {
    this.dirty = true
    this.schedule()
    for (const cb of this.listeners) cb()
  }

  /** Starts queued uploads up to `maxActive`; never re-enters itself. */
  private schedule(): void {
    if (this.scheduling) return
    this.scheduling = true
    try {
      const max = this.env.maxActive ?? 1
      let active = this.machines.filter((m) => !m.isIdle()).length
      for (const m of this.machines) {
        if (active >= max) break
        if (m.snapshot().state === 'queued') {
          m.start()
          active++
        }
      }
    } finally {
      this.scheduling = false
    }
  }
}

/** Browser wiring. Every global is read inside a factory, never at import. */
export function browserEnv(): EngineEnv {
  return {
    api: new HttpUploadApi(),
    put: xhrPutPart,
    md5: () => spawnMd5Worker(),
    sha256: () => spawnSha256Worker(),
    storage: () => new IdbStorage(),
    locks: () => navigator.locks as unknown as LockManagerLike,
    clock: {
      now: () => Date.now(),
      setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms) as unknown as number,
      clearTimeout: (handle) => globalThis.clearTimeout(handle),
    },
    random: Math.random,
    listen: (type, fn) => {
      if (type === 'visibilitychange') document.addEventListener(type, fn)
      else window.addEventListener(type, fn)
    },
    isVisible: () => document.visibilityState === 'visible',
  }
}

/** THE singleton. Import this from React; never construct another one. */
export const uploadEngine = new UploadEngine(browserEnv())
