/**
 * Whole-file SHA-256, streamed sequentially in its own worker so it runs in
 * parallel with the part uploads and never blocks them.
 * On resume it restarts from byte 0 of the re-selected file; complete waits on it.
 */
import { createSHA256 } from 'hash-wasm'
import { HASH_CHUNK_SIZE, type HashRequest, type HashResponse } from './hash'

let hasher: Awaited<ReturnType<typeof createSHA256>> | null = null
let suspended = false
let wake: (() => void) | null = null
/** One hasher instance ⇒ jobs must not interleave; queue them. */
let queue: Promise<void> = Promise.resolve()

function post(msg: HashResponse): void {
  postMessage(msg)
}

async function waitWhileSuspended(): Promise<void> {
  while (suspended) await new Promise<void>((resolve) => (wake = resolve))
}

async function run(req: Extract<HashRequest, { kind: 'hash' }>): Promise<void> {
  const chunkSize = req.chunk_size ?? HASH_CHUNK_SIZE
  hasher ??= await createSHA256()
  hasher.init()
  let chunks = 0
  for (let offset = req.start; offset < req.end; offset += chunkSize) {
    await waitWhileSuspended()
    const end = Math.min(offset + chunkSize, req.end)
    const buf = await req.blob.slice(offset, end).arrayBuffer()
    hasher.update(new Uint8Array(buf))
    chunks++
  }
  post({
    kind: 'done',
    id: req.id,
    hex: hasher.digest('hex'),
    bytes: req.end - req.start,
    chunks,
    scope: self.constructor.name,
  })
}

self.onmessage = (e: MessageEvent) => {
  const msg = e.data as HashRequest
  if (msg.kind === 'suspend') {
    suspended = true
    return
  }
  if (msg.kind === 'resume') {
    suspended = false
    wake?.()
    wake = null
    return
  }
  queue = queue
    .then(() => run(msg))
    .catch((err: unknown) => {
      hasher = null
      post({ kind: 'error', id: msg.id, message: err instanceof Error ? `${err.name}: ${err.message}` : String(err) })
    })
}
