/**
 * The share URLs this tab has minted, for as long as the tab lives.
 *
 * The server stores only a hash of the token, so the one time a URL is shown
 * is the moment it is made — after that nobody, owner included, can read it
 * back. This map is what lets the dialog and `/shared` still offer Copy for a
 * link made a minute ago in this tab. A module-scope singleton rather than a
 * context, like the upload engine: the dialog's tree and `/shared`'s do not
 * share an ancestor below the router, and a reload is supposed to forget.
 *
 * A share URL is a credential, so `AccountMenu` empties this wherever it
 * empties the query cache on sign-out.
 */

import { useSyncExternalStore } from 'react'

const urls = new Map<string, string>()
const listeners = new Set<() => void>()
let version = 0

function notify() {
  version += 1
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export const shareUrls = {
  get: (shareId: string): string | undefined => urls.get(shareId),
  set(shareId: string, url: string): void {
    urls.set(shareId, url)
    notify()
  },
  clear(): void {
    urls.clear()
    notify()
  },
}

/** The URL this tab holds for a share, re-rendering when one is minted or the map is emptied. */
export function useShareUrl(shareId: string): string | undefined {
  useSyncExternalStore(subscribe, () => version)
  return urls.get(shareId)
}
