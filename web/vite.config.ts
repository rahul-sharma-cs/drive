import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // `@/thing` → `web/src/thing`, matching tsconfig's `paths`. The replacement
  // is root-relative rather than the absolute `path.resolve(__dirname, './src')`
  // the shadcn CLI writes, because this file is type-checked by
  // tsconfig.node.json under `lib: ES2022` with no Node types installed — and
  // an alias is not worth a dependency. Only `@/…` matches; scoped packages
  // like `@tanstack/react-query` need the pattern plus a slash to be rewritten.
  resolve: {
    alias: {
      '@': '/src',
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
  build: {
    outDir: '../server/web/dist',
    emptyOutDir: true,
  },
})
