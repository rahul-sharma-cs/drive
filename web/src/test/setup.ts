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
