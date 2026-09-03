/**
 * Test setup, shared by both environments.
 *
 * Testing Library normally registers its own auto-cleanup, but only when a
 * global `afterEach` exists — and this suite runs with `globals: false`, so it
 * never does. Without this, mounted trees from one test are still in the
 * document during the next one and queries match the wrong element.
 *
 * The import is dynamic and guarded on `document`: setup files run for every
 * test file, and pulling react-dom into the engine suite's node environment
 * would be a browser dependency in the one place that must not have any.
 */

import { afterEach } from 'vitest'

afterEach(async () => {
  if (typeof document === 'undefined') return
  const { cleanup } = await import('@testing-library/react')
  cleanup()
})

/**
 * Node 22+ has a `localStorage` global of its own, usable only behind
 * `--localstorage-file` and `undefined` without it (reading it prints an
 * ExperimentalWarning). The jsdom environment leaves a global that already
 * exists alone, so under it a screen sees no storage at all — never jsdom's —
 * and nothing kept across a reload could be tested. A plain in-memory Storage
 * stands in, fresh per file, guarded on `document` so the engine suite's node
 * environment gains no browser global it did not ask for.
 */
if (typeof document !== 'undefined') {
  const items = new Map<string, string>()
  const storage: Storage = {
    get length() {
      return items.size
    },
    key: (index) => [...items.keys()][index] ?? null,
    getItem: (key) => items.get(key) ?? null,
    setItem: (key, value) => void items.set(key, String(value)),
    removeItem: (key) => void items.delete(key),
    clear: () => items.clear(),
  }
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true, writable: true })
}
