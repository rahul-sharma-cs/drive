import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

/**
 * The two faces every screen is set in, fetched at the same time as the
 * stylesheet that asks for them.
 *
 * Both are imported from CSS so that the bundler fingerprints them into
 * `assets/` — which is the only prefix the server sends `immutable` caching
 * for, and also the reason their file names are not knowable in advance. A
 * hand-written `<link rel=preload>` would have to carry a build hash, and a
 * stale hash preloads a file that does not exist while the real one is still
 * discovered late. So the links are written from the bundle itself, at the
 * moment the names exist.
 *
 * Latin only, deliberately: `latin-ext` sits behind its own `unicode-range`
 * and is fetched only by a page that actually renders one of those characters.
 * Preloading it would spend a request on almost every visit for almost none of
 * them.
 */
const LATIN_FACES = /^assets\/(instrument-sans-latin-wght-normal|ibm-plex-mono-latin-400-normal)-[\w-]+\.woff2$/

function preloadLatinFaces(): Plugin {
  return {
    name: 'drive-preload-latin-faces',
    apply: 'build',
    transformIndexHtml: {
      // After the bundle exists; there is nothing to read from before it does.
      order: 'post',
      handler(_html, ctx) {
        return Object.keys(ctx.bundle ?? {})
          .filter((file) => LATIN_FACES.test(file))
          .map((file) => ({
            tag: 'link',
            // `crossorigin` is not optional even same-origin: a font is always
            // fetched in CORS mode, and a preload without it is a second,
            // separate fetch rather than a warm cache entry.
            attrs: { rel: 'preload', as: 'font', type: 'font/woff2', crossorigin: '', href: `/${file}` },
            injectTo: 'head-prepend' as const,
          }))
      },
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss(), preloadLatinFaces()],
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
