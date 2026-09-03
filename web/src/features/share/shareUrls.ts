/**
 * The share URLs this browser has minted.
 *
 * The server stores only a hash of the token, so the one time a URL is shown
 * is the moment it is made — after that nobody, owner included, can read it
 * back from the server. This map is what lets the dialog and `/shared` still
 * offer Copy for a link made earlier in this browser. A module-scope
 * singleton rather than a context, like the upload engine: the dialog's tree
 * and `/shared`'s do not share an ancestor below the router.
 *
 * The map is mirrored to `localStorage` so that a reload comes back to it:
 * read once when the module loads, written on every `set`, removed on
 * `clear`. Every touch of storage sits in a `try/catch` — private mode, a
 * full store, no storage at all — and degrades to the in-memory map, which is
 * what there was before. The owner's own browser holding the owner's own
 * links is the clipboard's posture; the server-side posture (hash only) is
 * unchanged, so a database leak still leaks no link.
 *
 * A share URL is a credential, so `AccountMenu` empties this wherever it
 * empties the query cache on sign-out — and emptying it removes the stored
 * copy too.
 */

import { useSyncExternalStore } from 'react'

const STORAGE_KEY = 'drive.share-urls'

function load(): Map<string, string> {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === null) return new Map()
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return new Map()
    return new Map(
      Object.entries(parsed).filter((entry): entry is [string, string] => typeof entry[1] === 'string'),
    )
  } catch {
    return new Map()
  }
}

function persist() {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(Object.fromEntries(urls)))
  } catch {
    // Nowhere to keep it: this tab still holds the URL for as long as it lives.
  }
}

function forget() {
  try {
    localStorage.removeItem(STORAGE_KEY)
  } catch {
    // Nothing was kept there to begin with, or nothing can be touched.
  }
}

const urls = load()
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
    persist()
    notify()
  },
  clear(): void {
    urls.clear()
    forget()
    notify()
  },
}

/** The URL this browser holds for a share, re-rendering when one is minted or the map is emptied. */
export function useShareUrl(shareId: string): string | undefined {
  useSyncExternalStore(subscribe, () => version)
  return urls.get(shareId)
}
