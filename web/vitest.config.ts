import { defineConfig } from 'vitest/config'

// Two kinds of test live here and they want different environments.
//
// The upload engine is pure TypeScript — every browser dependency (XHR,
// IndexedDB, Web Locks, timers, RNG, hashing) is injected — so it runs in a
// node env and must keep running in one: the whole point of the injection is
// that no DOM is involved. Component tests need a DOM, and ask for one per file
// with an `@vitest-environment jsdom` docblock rather than flipping the default
// out from under the engine suite.
//
// This config deliberately does not load vite.config.ts's react/tailwind
// plugins: esbuild transforms TSX from tsconfig's `jsx: react-jsx` on its own,
// and nothing under test needs fast refresh or a stylesheet.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    globals: false,
    setupFiles: ['src/test/setup.ts'],
  },
})
