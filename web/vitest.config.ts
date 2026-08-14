import { defineConfig } from 'vitest/config'

// Plain vitest, node environment: the upload engine is pure TypeScript and
// every browser dependency (XHR, IndexedDB, Web Locks, timers, RNG, hashing)
// is injected, so no jsdom/happy-dom is needed. This config deliberately does
// not load vite.config.ts's react/tailwind plugins.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    globals: false,
  },
})
