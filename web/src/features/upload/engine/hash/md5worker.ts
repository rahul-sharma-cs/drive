/**
 * Per-part MD5, streamed over 8 MiB sub-slices — a 100 MiB part is never held
 * in memory. The digest is compared against the part PUT's normalized ETag;
 * an unnormalized compare always fails and would falsely downgrade integrity.
 */
import { createMD5 } from 'hash-wasm'
import { HASH_CHUNK_SIZE, type HashRequest, type HashResponse } from './hash'

let hasher: Awaited<ReturnType<typeof createMD5>> | null = null
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
  hasher ??= await createMD5()
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
      // A hash-wasm instantiation failure surfaces here, not as an error event.
      hasher = null
      post({ kind: 'error', id: msg.id, message: err instanceof Error ? `${err.name}: ${err.message}` : String(err) })
    })
}
