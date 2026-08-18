import { test, expect, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The core loop, in a real browser, interrupted for real.
 *
 * The Go `uploadclient` deliberately terminates rather than waiting out backend
 * downtime, so it cannot show this: resting in `paused_backend` and coming back
 * is the browser engine's half of the contract, and only a browser can
 * demonstrate it.
 *
 * The sequence is the product's whole reason to exist:
 *   upload → SIGKILL the server mid-transfer → the manager parks the upload
 *   → reload the page, which destroys the File handle for good → restart the
 *   server → the interrupted session is offered back → re-pick the SAME file
 *   → only the missing parts are PUT → the downloaded bytes hash equal to the
 *   source.
 *
 * It owns the server process, so it is deliberately NOT part of `make e2e`
 * (which runs one shared server for the whole suite). Run it with:
 *   make e2e-resume
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');
const BINARY = join(REPO, 'server', 'drive');
const FIXTURE_DIR = join(REPO, 'e2e', 'fixtures', 'resume');

const PART_SIZE = 10 * 1024 * 1024;
const PARTS = 12;
/** A short final part, so the uniform-size rule is exercised as in production. */
const FILE_SIZE = (PARTS - 1) * PART_SIZE + 3 * 1024 * 1024;

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = `http://localhost${env.DRIVE_ADDR}`;
const MAILPIT = env.DRIVE_MAILPIT_API;
/** Part PUTs go straight to the store; counting them proves what was re-sent. */
const STORE = env.DRIVE_S3_ENDPOINT;

let server: ChildProcess | null = null;

test.afterEach(() => stopServer());

test('an upload survives a server restart and resumes from the confirmed parts', async ({ page }) => {
  test.setTimeout(240_000);

  const fixture = ensureFixture();
  await startServer();
  const account = await createVerifiedAccount(page);

  const puts: string[] = [];
  page.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().startsWith(STORE)) puts.push(req.url());
  });

  await signIn(page, account);

  // --- upload, then kill the server mid-transfer -------------------------
  await page.getByLabel('Upload files').setInputFiles(fixture.path);
  const confirmedBeforeKill = await waitForConfirmedParts(page, 2);
  expect(confirmedBeforeKill).toBeGreaterThanOrEqual(2);
  expect(confirmedBeforeKill).toBeLessThan(PARTS);


  stopServer(); // SIGKILL: no graceful shutdown, exactly like a crash

  // The engine parks rather than failing — the API is unreachable while the
  // store may still be accepting parts (PLAN §Client engine state machine).
  await expect(page.getByText(/can't reach the server/i)).toBeVisible({ timeout: 60_000 });

  // --- leave the page (the File handle is gone), restart, come back -------
  // Navigating away first, while the server is still down, is what keeps the
  // interruption real: coming back to a live server would otherwise let the
  // engine's own `paused_backend` probe resume the upload in place, which is a
  // different (and weaker) proof than re-picking the file after a reload.
  await page.goto('about:blank');
  await startServer();
  await page.goto(BASE);

  const resumeRow = page.getByText(/parts are already uploaded/i);
  await expect(resumeRow).toBeVisible({ timeout: 30_000 });
  const offered = await resumeRow.textContent();
  const alreadyDone = Number(/Interrupted — (\d+) of/.exec(offered ?? '')?.[1]);
  expect(alreadyDone).toBeGreaterThanOrEqual(2);

  const sessions = await page.request.get(`${BASE}/api/uploads`);
  const active = ((await sessions.json()).items as Array<{ status: string; confirmed_parts: number[] }>).find(
    (s) => s.status === 'active',
  )!;
  const confirmedAtReload = new Set(active.confirmed_parts);
  expect(confirmedAtReload.size).toBe(alreadyDone);

  // Everything PUT up to this point belongs to the pre-crash attempt. Slicing
  // one list beats clearing it: Playwright delivers request events
  // asynchronously, and a PUT that landed just before the SIGKILL can be
  // reported after the kill returns.
  const beforeResume = puts.length;
  // The same file, unchanged on disk: the fingerprint covers name, size, mtime
  // and both edge blocks, so a regenerated file would silently start over.
  await page.getByLabel(`Pick ${fixture.name} to resume`).setInputFiles(fixture.path);

  await expect(page.getByText('Uploaded', { exact: true })).toBeVisible({ timeout: 180_000 });

  // Only the missing parts were sent. This is the assertion the whole feature
  // exists for: a resume that re-uploaded everything would still look green.
  //
  // The count can be BELOW `PARTS - alreadyDone`, and that is the resume
  // handshake working rather than a miscount: a part whose PUT reached the
  // store while the server was dying is confirmed by the ledger/ListParts
  // merge ("ListParts wins"), so the browser is never asked for it. What must
  // hold exactly is that no part the server already had is sent again, and
  // that the two sets together cover the file.
  const sentBeforeKill = new Set(puts.slice(0, beforeResume).map(partNumberOf));
  const resent = puts.slice(beforeResume).map(partNumberOf);
  expect(resent.filter((n) => confirmedAtReload.has(n))).toEqual([]);
  // Every part is accounted for: confirmed before the crash, sent before it
  // (and adopted by the merge), or sent now. Nothing was skipped, and the
  // resume sent strictly fewer parts than a fresh upload would have.
  expect(new Set([...resent, ...confirmedAtReload, ...sentBeforeKill]).size).toBe(PARTS);
  expect(resent.length).toBeLessThanOrEqual(PARTS - alreadyDone);
  expect(resent.length).toBeLessThan(PARTS);
  expect(sentBeforeKill.size).toBeGreaterThan(0);

  // --- the bytes came back byte-identical --------------------------------
  const children = await page.request.get(`${BASE}/api/nodes/${account.rootId}/children`);
  const items = (await children.json()).items as Array<{ id: string; name: string; size: number }>;
  const uploaded = items.find((n) => n.name === fixture.name);
  expect(uploaded, 'the uploaded file is listed in the folder').toBeTruthy();
  expect(uploaded!.size).toBe(FILE_SIZE);

  const download = await page.request.get(`${BASE}/api/files/${uploaded!.id}/download`);
  expect(download.status()).toBe(200);
  expect(download.headers()['content-disposition']).toContain('attachment');
  const digest = createHash('sha256').update(await download.body()).digest('hex');
  expect(digest).toBe(fixture.sha256);

  console.log(
    `resume proof: ${FILE_SIZE} bytes / ${PARTS} parts · ` +
      `${confirmedBeforeKill} confirmed when the server was killed · ` +
      `${alreadyDone} in the ledger at reload · ${resent.length} parts re-sent · sha256 match`,
  );
});

/* ------------------------------------------------------------------ helpers */

/** Presigned part URLs carry their part number in the query. */
function partNumberOf(url: string): number {
  return Number(new URL(url).searchParams.get('partNumber'));
}

function readEnvFile(path: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of readFileSync(path, 'utf8').split('\n')) {
    const trimmed = line.trim();
    if (trimmed === '' || trimmed.startsWith('#')) continue;
    const at = trimmed.indexOf('=');
    out[trimmed.slice(0, at)] = trimmed.slice(at + 1);
  }
  return out;
}

/** Deterministic bytes, written once and reused: mtime must survive the run. */
function ensureFixture(): { path: string; name: string; sha256: string } {
  const name = 'interrupted.bin';
  const path = join(FIXTURE_DIR, name);
  if (!existsSync(path)) {
    mkdirSync(FIXTURE_DIR, { recursive: true });
    const buf = Buffer.allocUnsafe(FILE_SIZE);
    let x = 0x1234_5678;
    for (let i = 0; i < FILE_SIZE; i++) {
      x ^= x << 13; x >>>= 0;
      x ^= x >>> 17;
      x ^= x << 5; x >>>= 0;
      buf[i] = x & 0xff;
    }
    writeFileSync(path, buf);
  }
  return { path, name, sha256: createHash('sha256').update(readFileSync(path)).digest('hex') };
}

async function startServer(): Promise<void> {
  server = spawn(BINARY, {
    env: { ...process.env, ...env, DRIVE_PART_SIZE: '10MiB' },
    stdio: 'ignore',
  });
  for (let i = 0; i < 120; i++) {
    try {
      const res = await fetch(`${BASE}/healthz`);
      if (res.ok) return;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`server did not become healthy at ${BASE}`);
}

function stopServer(): void {
  server?.kill('SIGKILL');
  server = null;
}

async function createVerifiedAccount(page: Page): Promise<{ email: string; password: string; rootId: string }> {
  const email = `resume-${randomUUID()}@example.test`;
  const password = 'a-long-enough-password-1';
  const headers = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

  const signup = await page.request.post(`${BASE}/api/auth/signup`, {
    headers,
    data: { email, password, display_name: 'Resume Test' },
  });
  expect(signup.status()).toBe(200);

  const token = await waitForVerificationToken(email);
  const verify = await page.request.post(`${BASE}/api/auth/verify-email`, { headers, data: { token } });
  expect(verify.status()).toBe(200);

  const login = await page.request.post(`${BASE}/api/auth/login`, { headers, data: { email, password } });
  expect(login.status()).toBe(200);
  const rootId = (await login.json()).root_id as string;
  // The UI signs in for itself below; this cookie is dropped first so the
  // login screen is genuinely exercised.
  await page.context().clearCookies();
  return { email, password, rootId };
}

/** Signup mails on a detached goroutine, so the inbox is polled, not read once. */
async function waitForVerificationToken(email: string): Promise<string> {
  for (let i = 0; i < 60; i++) {
    const list = await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent(`to:${email}`)}`);
    const messages = (await list.json()).messages as Array<{ ID: string }>;
    if (messages?.length) {
      const body = await (await fetch(`${MAILPIT}/api/v1/message/${messages[0].ID}`)).json();
      const found = /\/verify\?token=([^\s"<]+)/.exec(body.Text ?? '');
      if (found) return found[1];
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`no verification mail for ${email}`);
}

async function signIn(page: Page, account: { email: string; password: string }): Promise<void> {
  await page.goto(`${BASE}/login`);
  await page.getByLabel('Email').fill(account.email);
  await page.getByLabel('Password').fill(account.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();
}

/** Polls the server's own ledger — the UI's own text is not the evidence here. */
async function waitForConfirmedParts(page: Page, atLeast: number): Promise<number> {
  for (let i = 0; i < 600; i++) {
    const res = await page.request.get(`${BASE}/api/uploads`);
    if (res.ok()) {
      const sessions = (await res.json()).items as Array<{ status: string; confirmed_parts: number[] }>;
      const active = sessions.find((s) => s.status === 'active');
      if (active && active.confirmed_parts.length >= atLeast) return active.confirmed_parts.length;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`no upload reached ${atLeast} confirmed parts`);
}
