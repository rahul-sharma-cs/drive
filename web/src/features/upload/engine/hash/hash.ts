/**
 * Hash façade — the only thing the upload engine talks to.
 *
 * Two workers, both streaming (PLAN §Upload protocol: never materialize a whole
 * part in memory):
 *   - md5worker    — per-part MD5 over 8 MiB sub-slices, compared to the PUT's
 *                    normalized ETag.
 *   - sha256worker — the whole file, sequentially, in parallel with the uploads.
 *
 * Viability of hash-wasm's WASM *inside a Worker* is proven by
 * e2e/tests/hashwasm.spec.ts (PLAN §Fixed choices: first Phase 4a task).
 */

/** PLAN: hash a part in 8 MiB sub-slices, never as one buffer. */
export const HASH_CHUNK_SIZE = 8 * 1024 * 1024

export type HashRequest =
  | {
      kind: 'hash'
      id: number
      blob: Blob
      start: number
      end: number
      /** Test/tuning hook; defaults to HASH_CHUNK_SIZE inside the worker. */
      chunk_size?: number
    }
  | { kind: 'suspend' }
  | { kind: 'resume' }

export type HashResponse =
  | {
      kind: 'done'
      id: number
      hex: string
      bytes: number
      /** Number of sub-slices read — proves the streaming loop actually ran. */
      chunks: number
      /** globalThis constructor name — 'DedicatedWorkerGlobalScope' in a worker. */
      scope: string
    }
  | { kind: 'error'; id: number; message: string }

export type WorkerSpawn = () => Worker

/** One worker, one job at a time, plus pause/resume for the engine's pause. */
export class HashClient {
  private worker: Worker | null
  private seq = 0
  private pending = new Map<number, { resolve: (hex: string) => void; reject: (e: Error) => void }>()

  constructor(spawn: WorkerSpawn) {
    const worker = spawn()
    worker.onmessage = (e: MessageEvent) => {
      const msg = e.data as HashResponse
      const slot = this.pending.get(msg.id)
      if (!slot) return
      this.pending.delete(msg.id)
      if (msg.kind === 'done') slot.resolve(msg.hex)
      else slot.reject(new Error(msg.message))
    }
    worker.onerror = (e: ErrorEvent) => this.failAll(new Error(e.message || 'hash worker crashed'))
    this.worker = worker
  }

  /** Hex digest of blob[start,end); defaults to the whole blob. */
  hash(blob: Blob, start = 0, end = blob.size): Promise<string> {
    const worker = this.worker
    if (!worker) return Promise.reject(new Error('hash worker terminated'))
    const id = ++this.seq
    return new Promise<string>((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      worker.postMessage({ kind: 'hash', id, blob, start, end } satisfies HashRequest)
    })
  }

  /** Pause: the worker stops between sub-slices (PLAN: pause suspends both hash workers). */
  suspend(): void {
    this.worker?.postMessage({ kind: 'suspend' } satisfies HashRequest)
  }

  resume(): void {
    this.worker?.postMessage({ kind: 'resume' } satisfies HashRequest)
  }

  terminate(): void {
    this.worker?.terminate()
    this.worker = null
    this.failAll(new Error('hash worker terminated'))
  }

  private failAll(err: Error): void {
    for (const slot of this.pending.values()) slot.reject(err)
    this.pending.clear()
  }
}

export function spawnMd5Worker(): HashClient {
  return new HashClient(() => new Worker(new URL('./md5worker.ts', import.meta.url), { type: 'module' }))
}

export function spawnSha256Worker(): HashClient {
  return new HashClient(() => new Worker(new URL('./sha256worker.ts', import.meta.url), { type: 'module' }))
}
