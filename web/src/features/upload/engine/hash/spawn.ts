/**
 * Worker construction, kept apart from `hash.ts` on purpose.
 *
 * Both worker entry files import `hash.ts` for `HASH_CHUNK_SIZE` and the
 * message types. If the `new Worker(new URL('./…worker.ts'))` calls also lived
 * there, bundling either worker would pull in a reference to the other one and
 * Vite would reject the whole build: "Circular worker imports detected"
 * (md5worker → hash → sha256worker → hash → …). Splitting the URLs out of the
 * module the workers import breaks the cycle; nothing else changes.
 */

import { HashClient } from './hash'

export function spawnMd5Worker(): HashClient {
  return new HashClient(() => new Worker(new URL('./md5worker.ts', import.meta.url), { type: 'module' }))
}

export function spawnSha256Worker(): HashClient {
  return new HashClient(() => new Worker(new URL('./sha256worker.ts', import.meta.url), { type: 'module' }))
}
