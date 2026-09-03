import { test, expect, type Browser, type BrowserContext, type Locator, type Page } from '@playwright/test';
import { createHash, randomUUID } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { pdfBytes, pngBytes } from './support/fixtures';
import { readEnvFile } from './support/mailpit';

/**
 * Share links, end to end: an owner turns a file into a URL, strangers open it.
 *
 * The Go suite proves each route and the vitest suite proves each screen
 * against a stubbed client. What only this can say is that the chain holds
 * across the seam: the URL the dialog shows is one a browser with no account
 * can open, the password the owner typed is the one the gate wants, the
 * `<img>` on the page decodes bytes signed by the store, the counter the owner
 * reads is the one the recipient's downloads moved — and that a preview and a
 * reload moved it by nothing.
 *
 * Serial: one owner page and a few throwaway recipient contexts, each a
 * browser that has never seen the app. The link's URL is read off the dialog's
 * own input, never the clipboard — a headless clipboard proves nothing, and
 * the input is what a person copies from.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;

/** The seeded local account with the empty root. */
const ACCOUNT = { email: 'demo@drive.local', password: 'drive-demo-1' };
const CLIENT = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

const RUN = randomUUID().slice(0, 8);
const FIXTURE = `Share ${RUN}`;
/** Named per run: `/shared` is one list for the whole account, and a row is found by its file name. */
const PNG = { name: `photo-${RUN}.png`, mimeType: 'image/png', buffer: pngBytes(8) };
const TEXT_BODY = 'the words a recipient reads straight off the store\n'.repeat(4);
const TEXT = { name: `notes-${RUN}.txt`, mimeType: 'text/plain', buffer: Buffer.from(TEXT_BODY, 'utf8') };
const PDF = { name: `paper-${RUN}.pdf`, mimeType: 'application/pdf', buffer: pdfBytes('Drive') };
const PNG_SHA256 = sha256(PNG.buffer);

const PASSWORD = 'open-sesame-1';
const UNAVAILABLE = "This link isn't available. It may have expired or been turned off.";
const EXHAUSTED = 'This link has reached its download limit.';

let browserRef: Browser;
let owner: Page;
let fixtureId: string;
/** The photo's current link, as the chain replaces it. */
let url: string;
/** Recipients, each its own context: a browser that holds no account. */
let first: Page;
let second: Page;
let third: Page;
const strangers: BrowserContext[] = [];

test.describe.configure({ mode: 'serial' });
test.beforeEach(() => test.setTimeout(60_000));

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  browserRef = browser;
  owner = await browser.newPage();

  const login = await owner.request.post(`${BASE}/api/auth/login`, { headers: CLIENT, data: ACCOUNT });
  expect(login.status(), 'the seeded account signs in — did `make e2e` seed this stack?').toBe(200);
  const rootId = (await login.json()).root_id as string;
  const folder = await owner.request.post(`${BASE}/api/folders`, {
    headers: CLIENT,
    data: { parent_id: rootId, name: FIXTURE },
  });
  expect(folder.status()).toBe(201);
  fixtureId = (await folder.json()).id as string;

  await openFixture();
  await owner.getByLabel('Upload files').setInputFiles([PNG, TEXT, PDF]);
  for (const file of [PNG, TEXT, PDF]) await expect(rowNamed(file.name)).toBeVisible({ timeout: 60_000 });
  await owner.getByRole('button', { name: 'Clear finished' }).click();
  await expect(owner.getByRole('complementary', { name: 'Uploads' })).toBeHidden();
});

test.afterAll(async () => {
  for (const context of strangers) await context.close();
  await owner?.close();
});

/* ------------------------------------------------------------ the chain */

test('the owner makes a link behind a password with a cap of 2, and reads it off the dialog', async () => {
  const dialog = await openShare(PNG.name);
  await expect(dialog.getByLabel('Expires')).toHaveValue('7d');
  await dialog.getByLabel('Password', { exact: true }).fill(PASSWORD);
  await dialog.getByLabel('Download limit').fill('2');
  await dialog.getByRole('button', { name: 'Create link' }).click();

  url = await linkShown(dialog);
  await expect(dialog).toContainText('Password on · 0 of 2 downloads');
});

test('a stranger meets the gate, fails once, and then sees the image', async () => {
  first = await stranger();
  await first.goto(url);
  await expect(first.getByLabel('Password')).toBeVisible();
  await expect(first.getByRole('link', { name: 'Download' })).toHaveCount(0);

  await passGate(first, 'not-the-password');
  await expect(first.getByRole('alert')).toHaveText("That password didn't work.");
  await expect(first.getByLabel('Password')).toHaveValue('');

  await passGate(first, PASSWORD);
  await expectCard(first, PNG.name);
  await expectImage(first, PNG.name);
});

test('reloading the page spends nothing', async () => {
  for (let i = 0; i < 2; i++) {
    await first.reload();
    await passGate(first, PASSWORD);
    await expectImage(first, PNG.name);
  }
  await expectCount(PNG.name, '0 of 2');
});

test('Download follows the 302 to the bytes, and counts once per visitor', async () => {
  await download(first);
  await expectCount(PNG.name, '1 of 2');
  await download(first);
  await expectCount(PNG.name, '1 of 2');
});

test('a second stranger takes the last download, and a third meets the limit', async () => {
  second = await stranger();
  await second.goto(url);
  await passGate(second, PASSWORD);
  await expectCard(second, PNG.name);
  await download(second);
  await expectCount(PNG.name, '2 of 2');

  third = await stranger();
  await third.goto(url);
  await expect(third.getByRole('status')).toHaveText(EXHAUSTED);
  await expect(third.getByLabel('Password')).toHaveCount(0);
  await expect(third.getByRole('link', { name: 'Download' })).toHaveCount(0);
  await expect(third.locator('img')).toHaveCount(0);
});

test('raising the cap in Settings lets the third stranger in', async () => {
  const dialog = await openShare(PNG.name);
  // This tab reloaded since it made the link, so the URL is gone from it: New
  // link stands where Copy would, never a disabled Copy.
  await expect(dialog.getByRole('button', { name: 'New link' })).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Copy link' })).toHaveCount(0);

  await dialog.getByRole('button', { name: 'Settings' }).click();
  await dialog.getByLabel('Download limit').fill('3');
  await dialog.getByRole('button', { name: 'Save settings' }).click();
  await expect(dialog).toContainText('Password on · 2 of 3 downloads');

  await third.reload();
  await passGate(third, PASSWORD);
  await expectCard(third, PNG.name);
  await download(third);
  await expectCount(PNG.name, '3 of 3');
});

test('Stop sharing turns the link off', async () => {
  const dialog = await openShare(PNG.name);
  await dialog.getByRole('button', { name: 'Stop sharing' }).click();
  const confirm = owner.getByRole('dialog', { name: 'Stop sharing?' });
  await expect(confirm).toContainText('Anyone with the link loses access; downloads already started finish.');
  await confirm.getByRole('button', { name: 'Stop sharing' }).click();
  await expect(confirm).toBeHidden();
  await expect(dialog.getByRole('button', { name: 'Create link' })).toBeVisible();

  await first.reload();
  await expect(first.getByRole('status')).toHaveText(UNAVAILABLE);
  await expect(first.getByLabel('Password')).toHaveCount(0);
});

test('New link stops the old one and starts the count over', async () => {
  // A fresh link on the same file, open this time and capped at 5.
  const dialog = shareDialog(PNG.name);
  await dialog.getByLabel('Download limit').fill('5');
  await dialog.getByRole('button', { name: 'Create link' }).click();
  const before = await linkShown(dialog);
  expect(before).not.toBe(url);
  url = before;

  await first.goto(url);
  await expectCard(first, PNG.name);
  await expectImage(first, PNG.name);
  await download(first);
  await expectCount(PNG.name, '1 of 5');

  const again = await openShare(PNG.name);
  await again.getByRole('button', { name: 'New link' }).click();
  const confirm = owner.getByRole('dialog', { name: 'Make a new link?' });
  await expect(confirm).toContainText('The current link stops working, and the download count starts again at zero.');
  await confirm.getByRole('button', { name: 'New link' }).click();
  await expect(confirm).toBeHidden();
  const after = await linkShown(again);
  expect(after).not.toBe(before);
  await expect(again).toContainText('0 of 5 downloads');

  await first.goto(before);
  await expect(first.getByRole('status')).toHaveText(UNAVAILABLE);
  url = after;
  await first.goto(url);
  await expectImage(first, PNG.name);
  await expectCount(PNG.name, '0 of 5');
});

test('a trashed file makes its link inert until it is restored', async () => {
  await openFixture();
  await rowNamed(PNG.name).click({ button: 'right' });
  await contextMenu().getByRole('menuitem', { name: 'Move to trash' }).click();
  await expect(rowNamed(PNG.name)).toHaveCount(0);

  await first.reload();
  await expect(first.getByRole('status')).toHaveText(UNAVAILABLE);
  await openShared();
  await expect(sharedRow(PNG.name)).toContainText('In trash');

  await owner.goto(`${BASE}/trash`);
  await expect(owner.getByRole('heading', { name: 'Trash' })).toBeVisible();
  await rowNamed(PNG.name).click({ button: 'right' });
  await contextMenu().getByRole('menuitem', { name: 'Restore' }).click();
  await expect(rowNamed(PNG.name)).toHaveCount(0);

  await first.reload();
  await expectImage(first, PNG.name);
  await download(first);
  await expectCount(PNG.name, '1 of 5');
});

test('a text file is shown as its text', async () => {
  const dialog = await openShare(TEXT.name);
  await dialog.getByRole('button', { name: 'Create link' }).click();
  const link = await linkShown(dialog);

  const reader = await stranger();
  await reader.goto(link);
  await expectCard(reader, TEXT.name);
  await expect(reader.locator('pre')).toContainText('the words a recipient reads straight off the store');
});

test('a PDF gets the card and the button only', async () => {
  const dialog = await openShare(PDF.name);
  await dialog.getByRole('button', { name: 'Create link' }).click();
  const link = await linkShown(dialog);

  const reader = await stranger();
  const asked: string[] = [];
  reader.on('request', (request) => {
    if (request.url().includes('/preview')) asked.push(request.url());
  });
  await reader.goto(link);
  await expectCard(reader, PDF.name);
  // A beat after the card, because "never asked" is a claim about a request
  // that would arrive slightly after the element it belongs to.
  await reader.waitForTimeout(500);
  expect(asked, 'the page never asks for a preview it would be refused').toEqual([]);
  await expect(reader.locator('img, iframe, video, audio, pre')).toHaveCount(0);
});

test('/shared lists every link with its counts', async () => {
  await openShared();
  await expect(owner.getByRole('columnheader', { name: 'Expires' })).toBeVisible();
  await expect(owner.getByRole('columnheader', { name: 'Downloads' })).toBeVisible();

  const photo = sharedRow(PNG.name);
  await expect(photo.getByRole('cell', { name: '1 of 5', exact: true })).toBeVisible();
  await expect(photo.getByRole('cell', { name: 'Expires in 7 days', exact: true })).toBeVisible();
  await expect(sharedRow(TEXT.name).getByRole('cell', { name: '0', exact: true })).toBeVisible();
  await expect(sharedRow(PDF.name).getByRole('cell', { name: '0', exact: true })).toBeVisible();
});

/* --------------------------------------------------------------- helpers */

function sha256(bytes: Buffer | Uint8Array): string {
  return createHash('sha256').update(bytes).digest('hex');
}

/** A browser that has never seen the app: no cookie, no account. */
async function stranger(): Promise<Page> {
  const context = await browserRef.newContext();
  strangers.push(context);
  return context.newPage();
}

async function openFixture(): Promise<void> {
  await owner.goto(`${BASE}/folders/${fixtureId}`);
  await expect(owner.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]')).toHaveText(
    FIXTURE,
  );
}

/** The row menu's Share, for a file in the fixture folder. */
async function openShare(name: string): Promise<Locator> {
  await openFixture();
  await rowNamed(name).click({ button: 'right' });
  await contextMenu().getByRole('menuitem', { name: 'Share' }).click();
  const dialog = shareDialog(name);
  await expect(dialog).toBeVisible();
  return dialog;
}

/** The URL the dialog shows once, read off its input: `<base>/s/<43 base64url characters>`. */
async function linkShown(dialog: Locator): Promise<string> {
  const input = dialog.getByLabel('Link', { exact: true });
  await expect(input).toBeVisible();
  const value = await input.inputValue();
  expect(value.startsWith(`${BASE}/s/`), `${value} is a link into this deployment`).toBe(true);
  expect(value.slice(`${BASE}/s/`.length)).toMatch(/^[A-Za-z0-9_-]{43}$/);
  return value;
}

async function passGate(page: Page, password: string): Promise<void> {
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Open' }).click();
}

async function expectCard(page: Page, name: string): Promise<void> {
  await expect(page.getByRole('heading', { level: 1, name })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Download' })).toBeVisible();
}

/** The image decoded, not merely an `<img>` in the DOM: the bytes came back from the store and are a PNG. */
async function expectImage(page: Page, name: string): Promise<void> {
  const image = page.getByRole('img', { name });
  await expect(image).toBeVisible();
  await expect.poll(() => image.evaluate((el) => (el as HTMLImageElement).naturalWidth)).toBeGreaterThan(0);
}

/**
 * Download through the page's own cookie jar: the 302 to the presigned GET is
 * followed, and the body hashes equal to what the owner uploaded.
 */
async function download(page: Page): Promise<void> {
  const href = await page.getByRole('link', { name: 'Download' }).getAttribute('href');
  expect(href).toMatch(/^\/api\/s\/[A-Za-z0-9_-]{43}\/download$/);
  const res = await page.request.get(`${BASE}${href}`);
  expect(res.status()).toBe(200);
  expect(res.headers()['content-disposition']).toContain('attachment');
  expect(sha256(await res.body())).toBe(PNG_SHA256);
}

async function openShared(): Promise<void> {
  await owner.goto(`${BASE}/shared`);
  await expect(owner.getByRole('heading', { name: 'Shared links' })).toBeVisible();
}

/** What the owner reads on `/shared` for a file's link. */
async function expectCount(name: string, downloads: string): Promise<void> {
  await openShared();
  await expect(sharedRow(name).getByRole('cell', { name: downloads, exact: true })).toBeVisible();
}

const rows = () => owner.locator('[data-testid="file-row"]');
const rowNamed = (name: string) => rows().filter({ hasText: name });
const contextMenu = () => owner.locator('[data-slot="context-menu-content"]');
const shareDialog = (name: string) => owner.getByRole('dialog', { name: `Share "${name}"` });
const sharedRow = (name: string) => owner.getByTestId('share-row').filter({ hasText: name });
