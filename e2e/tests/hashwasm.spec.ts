import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';

/**
 * hash-wasm viability. hash-wasm is pinned at exactly 4.12.0 and has been
 * dormant upstream since Nov 2024 — expect no fixes — so WASM instantiation
 * must be proven INSIDE A REAL WEB WORKER before the upload engine is built on
 * it. It stays as the permanent regression guard.
 *
 * Self-contained: bundles the real worker sources with esbuild, serves them
 * plus a deterministic fixture from an in-process HTTP server on an ephemeral
 * port. Needs no drive server, no docker stacks, and no playwright.config change.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');
const ENGINE_HASH_DIR = join(REPO, 'web', 'src', 'features', 'upload', 'engine', 'hash');
const ESBUILD = join(REPO, 'web', 'node_modules', '.bin', 'esbuild');

const CHUNK = 8 * 1024 * 1024;       // the engine's sub-slice size: a part is hashed in
                                     // 8 MiB pieces, never materialized as one buffer
const FIXTURE_SIZE = 20 * 1024 * 1024; // 8 + 8 + 4 MiB => 3 chunks, last one short

/** Deterministic, non-degenerate bytes: all-zeros would hide chunk-ordering bugs. */
function fixtureBytes(size: number): Buffer {
  const buf = Buffer.allocUnsafe(size);
  let x = 0x9e3779b9;
  for (let i = 0; i < size; i++) {
    x ^= x << 13; x >>>= 0;
    x ^= x >>> 17;
    x ^= x << 5; x >>>= 0;
    buf[i] = x & 0xff;
  }
  return buf;
}

type Done = { kind: string; hex: string; bytes: number; chunks: number; scope: string; blob_size: number; ms: number };

let server: Server;
let origin: string;
let outDir: string;
const fixture = fixtureBytes(FIXTURE_SIZE);
const expectedMd5 = createHash('md5').update(fixture).digest('hex');
const expectedSha256 = createHash('sha256').update(fixture).digest('hex');

test.beforeAll(async () => {
  outDir = mkdtempSync(join(tmpdir(), 'drive-hashwasm-'));
  const bundles = new Map<string, Buffer>();
  for (const name of ['md5worker', 'sha256worker']) {
    const out = join(outDir, `${name}.js`);
    execFileSync(ESBUILD, [join(ENGINE_HASH_DIR, `${name}.ts`), '--bundle', '--format=iife', `--outfile=${out}`], {
      stdio: 'pipe',
    });
    bundles.set(`/${name}.js`, readFileSync(out));
  }
  const page = readFileSync(join(REPO, 'e2e', 'hashwasm-page', 'index.html'));

  server = createServer((req, res) => {
    const path = (req.url ?? '/').split('?')[0];
    if (path === '/' || path === '/index.html') {
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' }).end(page);
    } else if (path === '/fixture.bin') {
      res.writeHead(200, { 'content-type': 'application/octet-stream', 'content-length': String(fixture.length) }).end(fixture);
    } else if (bundles.has(path)) {
      res.writeHead(200, { 'content-type': 'text/javascript; charset=utf-8' }).end(bundles.get(path));
    } else {
      res.writeHead(404).end('not found');
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  origin = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

test.afterAll(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
  rmSync(outDir, { recursive: true, force: true });
});

test('hash-wasm instantiates inside a Worker and streams correct digests', async ({ page }) => {
  // Count any WASM use on the MAIN thread: it must stay at zero, so a passing
  // digest can only have come from WASM running inside the worker.
  await page.addInitScript(() => {
    (window as any).__mainThreadWasm = 0;
    for (const fn of ['instantiate', 'instantiateStreaming', 'compile', 'compileStreaming'] as const) {
      const orig = (WebAssembly as any)[fn];
      if (typeof orig !== 'function') continue;
      (WebAssembly as any)[fn] = (...args: unknown[]) => {
        (window as any).__mainThreadWasm++;
        return orig.apply(WebAssembly, args);
      };
    }
  });

  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error') pageErrors.push(m.text()); });

  await page.goto(`${origin}/`);

  const run = (script: string, chunkSize: number) =>
    page.evaluate(
      ([s, c]) => (window as any).__hashRun(s, c) as Promise<Done>,
      [script, chunkSize] as [string, number],
    ) as Promise<Done>;

  // 1. Streaming MD5 in 8 MiB sub-slices, in a worker.
  const md5Chunked = await run('/md5worker.js', CHUNK);
  expect(md5Chunked.scope).toBe('DedicatedWorkerGlobalScope');
  expect(md5Chunked.blob_size).toBe(FIXTURE_SIZE);
  expect(md5Chunked.bytes).toBe(FIXTURE_SIZE);
  expect(md5Chunked.chunks).toBe(3); // 8 + 8 + 4 MiB — the short final slice is real
  expect(md5Chunked.hex).toBe(expectedMd5);

  // 2. Chunked == one-go: same worker, one slice covering the whole blob.
  const md5OneGo = await run('/md5worker.js', FIXTURE_SIZE);
  expect(md5OneGo.chunks).toBe(1);
  expect(md5OneGo.hex).toBe(md5Chunked.hex);

  // 3. The SHA-256 worker matches node:crypto over the same bytes.
  const sha = await run('/sha256worker.js', CHUNK);
  expect(sha.scope).toBe('DedicatedWorkerGlobalScope');
  expect(sha.chunks).toBe(3);
  expect(sha.hex).toBe(expectedSha256);

  // 4. Nothing instantiated WASM on the main thread.
  expect(await page.evaluate(() => (window as any).__mainThreadWasm)).toBe(0);
  expect(pageErrors).toEqual([]);

  console.log(
    `hash-wasm 4.12.0 in ${md5Chunked.scope}: md5 ${md5Chunked.ms}ms (${md5Chunked.chunks} chunks), ` +
      `md5 one-go ${md5OneGo.ms}ms, sha256 ${sha.ms}ms over ${FIXTURE_SIZE} bytes`,
  );
});
