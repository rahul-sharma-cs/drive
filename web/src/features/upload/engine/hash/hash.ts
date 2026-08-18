/**
 * Hash façade — the only thing the upload engine talks to.
 *
 * Two workers, both streaming — a whole part is never materialized in memory,
 * which is what keeps a 100 MiB part size from costing 100 MiB of heap per
 * upload in flight:
 *   - md5worker    — per-part MD5 over 8 MiB sub-slices, compared to the PUT's
 *                    normalized ETag.
 *   - sha256worker — the whole file, sequentially, in parallel with the uploads.
 *
 * Viability of hash-wasm's WASM *inside a Worker* is proven by
 * e2e/tests/hashwasm.spec.ts — hash-wasm is pinned at 4.12.0 and dormant
 * upstream, so that check is a standing guard rather than a one-off.
 */

/** A part is hashed in 8 MiB sub-slices, never as one buffer. */
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

  /**
   * Pause: the worker stops between sub-slices. Pausing an upload suspends both
   * hash workers, not just the transfer — otherwise a paused upload keeps
   * burning CPU.
   */
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

// The two spawners live in ./spawn.ts, NOT here. Both worker files import this
// module for HASH_CHUNK_SIZE and the message types, so a `new Worker(new
// URL('./md5worker.ts'))` in this file makes the worker graph circular
// (md5worker -> hash -> sha256worker -> hash -> ...) and Vite refuses to bundle
// it. Nothing catches that until the engine is imported by the app.
