import { test, expect } from '@playwright/test';
import { readFileSync, writeFileSync } from 'node:fs';

/**
 * Day-0 spike — browser half. The spike is what proved browser->Garage direct
 * uploads work at all: presigned part PUTs from a page context, single-valued
 * CORS headers, readable ETags, and the expired-presign error shape.
 *
 * Driven by `go run ./server/cmd/spike`, which serves this page's directory on
 * http://localhost:5173 (Playwright's default about:blank has origin "null" and
 * matches no CORS rule), writes SPIKE_MANIFEST, then reads SPIKE_RESULTS back.
 * Run it via `make spike`, not directly.
 */

const MANIFEST = process.env.SPIKE_MANIFEST;
const RESULTS = process.env.SPIKE_RESULTS;
const PAGE_URL = process.env.SPIKE_PAGE_URL ?? 'http://localhost:5173/index.html';

type Manifest = {
  s3_endpoint: string;
  drop_dir: string;
  drop_dir_file_count: number;
  parts: { part_number: number; url: string; file: string; size: number; md5: string; content_type: string }[];
  expired_put_url: string;
  expired_put_key: string;
};

test('presigned part PUTs from a real page origin', async ({ page }) => {
  if (!MANIFEST || !RESULTS) throw new Error('SPIKE_MANIFEST and SPIKE_RESULTS must be set (run via `make spike`)');
  const manifest: Manifest = JSON.parse(readFileSync(MANIFEST, 'utf8'));

  // Raw response headers are only visible through CDP: Access-Control-Allow-Origin
  // is not a JS-readable response header, and the two-rule CORS fix is exactly
  // about it carrying ONE origin. CDP joins duplicate headers with "\n".
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Network.enable');
  const requests = new Map<string, { url: string; method: string }>();
  const extraInfo: { requestId: string; headers: Record<string, string>; statusCode: number }[] = [];
  cdp.on('Network.requestWillBeSent', (e: any) =>
    requests.set(e.requestId, { url: e.request.url, method: e.request.method }));
  cdp.on('Network.responseReceivedExtraInfo', (e: any) =>
    extraInfo.push({ requestId: e.requestId, headers: e.headers, statusCode: e.statusCode }));

  const consoleErrors: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });

  await page.goto(PAGE_URL);
  const put = await page.evaluate(() => (window as any).__spikeRun());

  // Correlate CDP header records back to the S3 endpoint.
  const s3Host = new URL(manifest.s3_endpoint).host;
  const cors = extraInfo
    .map((e) => ({ ...e, req: requests.get(e.requestId) }))
    .filter((e) => e.req && new URL(e.req.url).host === s3Host)
    .map((e) => {
      const acaoRaw = e.headers['access-control-allow-origin'] ?? e.headers['Access-Control-Allow-Origin'] ?? null;
      return {
        method: e.req!.method,
        url: e.req!.url.split('?')[0],
        status: e.statusCode,
        acao_raw: acaoRaw,
        // Two rules merged into one header shows up either as a duplicated
        // header (CDP joins with \n) or as one comma-joined value.
        acao_value_count: acaoRaw === null ? 0 : acaoRaw.split(/[\n,]/).map((s) => s.trim()).filter(Boolean).length,
        expose_headers: e.headers['access-control-expose-headers']
          ?? e.headers['Access-Control-Expose-Headers'] ?? null,
      };
    });

  // The expired-presign PUT is deliberately refused, and a refusal carries no
  // CORS headers at all — that opacity IS the measurement. Keep it out of the
  // one-origin assertion below, which is about the successful part PUTs.
  const isExpiredProbe = (url: string) => url.includes(manifest.expired_put_key);
  const preflights = cors.filter((c) => c.method === 'OPTIONS' && !isExpiredProbe(c.url));
  const puts = cors.filter((c) => c.method === 'PUT' && !isExpiredProbe(c.url));

  const expiredProbe = cors.filter((c) => isExpiredProbe(c.url));
  writeFileSync(RESULTS, JSON.stringify({ put, cors, preflights, puts, expired_probe: expiredProbe, console_errors: consoleErrors }, null, 2));

  // --- assertions -----------------------------------------------------------
  expect(put.fatal, 'in-page fatal error').toBeNull();
  expect(put.parts.length).toBe(manifest.parts.length);
  for (const p of put.parts) {
    expect(p.error, `part ${p.part_number} transport error`).toBeNull();
    expect(p.status, `part ${p.part_number} status`).toBe(200);
    expect(p.etag_readable, `part ${p.part_number} ETag readable (CORS ExposeHeaders)`).toBe(true);
    expect(p.md5_match, `part ${p.part_number} normalized ETag == client MD5`).toBe(true);
  }
  // A part sent with an explicit Content-Type must have gone through a preflight
  // and still succeeded — the proof that the rule's AllowedHeaders covers what
  // the engine sends. Enumerating "content-type" suffices; no wildcard needed.
  expect(put.parts.some((p: any) => p.sent_content_type !== null && p.status === 200)).toBe(true);

  // The two-rule CORS workaround: every CORS response carries exactly one origin.
  expect(puts.length, 'CDP saw the part PUTs').toBeGreaterThan(0);
  for (const c of [...puts, ...preflights]) {
    expect(c.acao_value_count, `${c.method} ${c.url} Access-Control-Allow-Origin value count`).toBe(1);
  }
});

test('CDP folder-drop viability', async ({ page }) => {
  if (!MANIFEST || !RESULTS) throw new Error('SPIKE_MANIFEST and SPIKE_RESULTS must be set (run via `make spike`)');
  const manifest: Manifest = JSON.parse(readFileSync(MANIFEST, 'utf8'));

  await page.goto(PAGE_URL);
  const cdp = await page.context().newCDPSession(page);

  const box = (await page.locator('#dropzone').boundingBox())!;
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  const data = {
    items: [{ mimeType: 'text/uri-list', data: `file://${manifest.drop_dir}` }],
    files: [manifest.drop_dir],
    dragOperationsMask: 1,
  };

  let probe: any = { supported: false, note: 'not attempted' };
  try {
    for (const type of ['dragEnter', 'dragOver', 'drop'] as const) {
      await cdp.send('Input.dispatchDragEvent', { type, x, y, data } as any);
    }
    await page.waitForFunction(() => (window as any).__SPIKE_DROP_RESULT__ !== undefined, undefined, { timeout: 5000 });
    const r = await page.evaluate(() => (window as any).__SPIKE_DROP_RESULT__);
    probe = {
      ...r,
      supported: r.entry_kinds?.includes('directory') === true,
      children_match: r.directory_children === manifest.drop_dir_file_count,
      expected_children: manifest.drop_dir_file_count,
      note: r.entry_kinds?.includes('directory')
        ? 'CDP folder drop yields a directory entry — drag-drop ingress is viable'
        : 'CDP folder drop did NOT yield a directory entry — fallback trigger: use the webkitdirectory picker for e2e folder ingress',
    };
  } catch (e: any) {
    probe = { supported: false, error: String(e?.message ?? e), note: 'Input.dispatchDragEvent failed — fallback trigger: webkitdirectory picker' };
  }

  const prev = JSON.parse(readFileSync(RESULTS, 'utf8'));
  writeFileSync(RESULTS, JSON.stringify({ ...prev, folder_drop: probe }, null, 2));

  // Deliberately NOT a hard failure: a failed synthesized folder drop is the
  // trigger to switch e2e folder ingress to the webkitdirectory picker, not a
  // bug to debug. Both ingress paths share the same traverse core, so the
  // fallback still exercises the logic under test. The Go harness records the
  // verdict.
  test.info().annotations.push({ type: 'folder-drop', description: JSON.stringify(probe) });
});
