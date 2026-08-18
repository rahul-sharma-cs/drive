/**
 * The React side of the upload engine.
 *
 * The engine is a module-scope singleton living outside the React tree, so a
 * route change never unmounts an upload. `subscribe` and `getSnapshot` are both
 * pre-bound and identity-stable, and `getSnapshot()` returns a cached object
 * whose identity only changes when something actually changed — exactly what
 * `useSyncExternalStore` requires. Never construct a second `UploadEngine`.
 */

import { useSyncExternalStore } from 'react'

import { uploadEngine } from '../engine/engine'
import type { ConflictPolicy, UploadSnapshot } from '../engine/types'

export function useUploadItems(): UploadSnapshot[] {
  return useSyncExternalStore(uploadEngine.subscribe, uploadEngine.getSnapshot).items
}

/**
 * Actions, bound once at module scope. The engine's methods are ordinary class
 * methods (unbound), and an arrow created per render would give every memoized
 * row a new prop on every snapshot — which is the thing the memo exists to
 * prevent, since a 150-file drop re-mints all 150 row objects per part confirm.
 */
export const uploadActions = {
  enqueue: (file: File, parentId: string) => uploadEngine.enqueue(file, parentId),
  pause: (id: string) => uploadEngine.pause(id),
  resume: (id: string) => uploadEngine.resume(id),
  retry: (id: string) => uploadEngine.retry(id),
  cancel: (id: string) => void uploadEngine.cancel(id),
  reselect: (id: string, file: File) => uploadEngine.reselect(id, file),
  resolveConflict: (id: string, policy: ConflictPolicy) => uploadEngine.resolveConflict(id, policy),
  clearFinished: () => uploadEngine.clearFinished(),
}

export type UploadActions = typeof uploadActions
