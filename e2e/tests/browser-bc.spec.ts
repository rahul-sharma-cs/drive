import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type Locator,
  type Page,
} from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateSync } from 'node:zlib';

/**
 * Sorting, folder counts, the trash's bulk commands and the preview viewer —
 * in a real browser, against a real server and a real object store.
 *
 * What is only provable here is the *seam*. The Go suite proves the endpoints
 * answer; the vitest suite proves the screens render what a stubbed client
 * hands them. Neither can say that a click on the Size header ends in a
 * request the server actually sorts by, that a folder row quotes a count the
 * database counted, that three rows restored out of the trash come back into
 * the folder they left, or — the one that matters most — that a type the
 * server refuses to sign a link for causes *no request to the object store at
 * all*. That last claim is about a network the unit suites do not have.
 *
 * It runs inside `make e2e`, against that run's shared server and its freshly
 * seeded database, signed in as the seeded local account. Every fixture is
 * named with a per-run id, so a second run against a stack that was not
 * reseeded does not collide with the first.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;
const APP_ORIGIN = new URL(BASE).origin;

/**
 * The seeded local account: the one with an empty root. `rahul@drive.local`
 * owns the sample tree the Go and search tests read, and this spec empties a
 * trash.
 */
const ACCOUNT = { email: 'demo@drive.local', password: 'drive-demo-1' };

const RUN = randomUUID().slice(0, 8);

const CLIENT = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

/** One signed-in page for the whole file: signing in once per case proves nothing extra. */
let page: Page;
/** A cookie-less context for reading presigned URLs, so no app cookie rides along. */
let store: APIRequestContext;

/* ------------------------------------------------------- fixture bytes */

/**
 * A text file of an exact size, so the size column and the size sort have
 * something unambiguous to order.
 */
function filler(name: string, size: number): Buffer {
  const line = `${name} — a fixture for the sort cases\n`;
  return Buffer.from(line.repeat(Math.ceil(size / line.length)), 'utf8').subarray(0, size);
}

/** Built on first use rather than at module scope, which keeps the order of declarations here free. */
let crcTable: Uint32Array | undefined;

function crc32(buf: Buffer): number {
  if (crcTable === undefined) {
    crcTable = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      crcTable[n] = c >>> 0;
    }
  }
  let c = 0xffffffff;
  for (const byte of buf) c = crcTable[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type: string, data: Buffer): Buffer {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([length, body, crc]);
}

/**
 * A real PNG, built rather than pasted: the viewer's `<img>` reports an error
 * on bytes it cannot decode and falls back to the download card, so a fixture
 * that merely claimed to be a PNG would quietly test the wrong branch.
 */
function pngBytes(side: number): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(side, 0);
  ihdr.writeUInt32BE(side, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolour
  const stride = 1 + side * 3;
  const raw = Buffer.alloc(side * stride);
  for (let y = 0; y < side; y++) {
    for (let x = 0; x < side; x++) {
      const at = y * stride + 1 + x * 3;
      raw[at] = (x * 24) % 256;
      raw[at + 1] = (y * 24) % 256;
      raw[at + 2] = 0x80;
    }
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

/** A one-page PDF with a real cross-reference table, offsets computed as it is built. */
function pdfBytes(text: string): Buffer {
  const stream = `BT /F1 18 Tf 20 60 Td (${text}) Tj ET\n`;
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 240 120] /Contents 4 0 R'
      + ' /Resources << /Font << /F1 5 0 R >> >> >>',
    `<< /Length ${Buffer.byteLength(stream, 'latin1')} >>\nstream\n${stream}endstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ];

  let body = '%PDF-1.4\n';
  const offsets: number[] = [];
  objects.forEach((object, index) => {
    offsets.push(Buffer.byteLength(body, 'latin1'));
    body += `${index + 1} 0 obj\n${object}\nendobj\n`;
  });

  const xrefAt = Buffer.byteLength(body, 'latin1');
  body += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  for (const offset of offsets) body += `${String(offset).padStart(10, '0')} 00000 n \n`;
  body += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xrefAt}\n%%EOF\n`;
  return Buffer.from(body, 'latin1');
}

/**
 * A 24-bit bitmap. Nothing decodes it — the point of this fixture is that it is
 * an image type the server's allowlist does not carry, so the viewer never asks
 * the store for it — but a fixture that lied about being one would be a worse
 * test of that than one that does not.
 */
function bmpBytes(side: number): Buffer {
  const rowBytes = Math.ceil((side * 3) / 4) * 4;
  const pixels = Buffer.alloc(rowBytes * side, 0x40);
  const header = Buffer.alloc(54);
  header.write('BM', 0, 'ascii');
  header.writeUInt32LE(header.length + pixels.length, 2);
  header.writeUInt32LE(header.length, 10);
  header.writeUInt32LE(40, 14); // DIB header size
  header.writeInt32LE(side, 18);
  header.writeInt32LE(side, 22);
  header.writeUInt16LE(1, 26); // planes
  header.writeUInt16LE(24, 28); // bits per pixel
  header.writeUInt32LE(pixels.length, 34);
  return Buffer.concat([header, pixels]);
}

/* ------------------------------------------------------------------ fixtures */

/**
 * The sort fixture. Two folders — one holding two things, one holding nothing,
 * which is what the count column has to say out loud — and three files whose
 * name order, size order and upload order are all different from each other.
 * A sort that was ignored would land on the name order every time, so three
 * distinct orders is what makes each case a claim rather than a coincidence.
 */
const SORTED = `Sorted ${RUN}`;
const FULL_FOLDER = 'Archive';
const EMPTY_FOLDER = 'Backup';
/** Uploaded in this order, one at a time, so `updated_at` ascends with the array. */
const SORT_FILES = [
  { name: 'quince.txt', size: 2048 },
  { name: 'fig.txt', size: 3072 },
  { name: 'pear.txt', size: 1024 },
] as const;

const BY_NAME = ['fig.txt', 'pear.txt', 'quince.txt'];
const BY_SIZE_ASC = ['pear.txt', 'quince.txt', 'fig.txt'];
const BY_SIZE_DESC = ['fig.txt', 'quince.txt', 'pear.txt'];
/** Newest first: the reverse of the upload order above. */
const BY_UPDATED_DESC = ['pear.txt', 'fig.txt', 'quince.txt'];

/**
 * The preview fixture, one file per branch of the viewer plus a folder.
 *
 * `cherry` sorts by name between `banana.txt` and `date.pdf`, which is where
 * the arrows would run into it if the viewer walked rows rather than files.
 * (The server ranks folders above files in both directions, so it renders
 * first — "between" is about the name order, and the claim under test is that
 * the sibling walk is files-only.)
 *
 * `elder.svg` is the type the design refuses on purpose. `sketch.bmp` is an
 * image type that is simply not on the server's allowlist, and it is the one
 * that makes the no-request-to-the-store assertion bite: an SVG is refused
 * twice over — by the server's allowlist and again by `previewKind` — so a
 * server that echoed the stored MIME back would still not move it.
 */
const PREVIEWS = `Preview ${RUN}`;
const MIDDLE_FOLDER = 'cherry';
const TEXT_BODY = 'the file the viewer reads straight off the store\n'.repeat(6);

interface Fixture {
  name: string;
  mimeType: string;
  buffer: Buffer;
}

const PREVIEW_FILES: Fixture[] = [
  { name: 'apple.png', mimeType: 'image/png', buffer: pngBytes(8) },
  { name: 'banana.txt', mimeType: 'text/plain', buffer: Buffer.from(TEXT_BODY, 'utf8') },
  { name: 'date.pdf', mimeType: 'application/pdf', buffer: pdfBytes('Drive') },
  {
    name: 'elder.svg',
    mimeType: 'image/svg+xml',
    buffer: Buffer.from(
      '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16" fill="#0f766e"/></svg>\n',
      'utf8',
    ),
  },
  { name: 'sketch.bmp', mimeType: 'image/bmp', buffer: bmpBytes(2) },
];

/** The trash fixture: four folders, so a bulk command can leave some behind. */
const TRASHED = `Trash ${RUN}`;
const BOXES = ['Box A', 'Box B', 'Box C', 'Box D'] as const;

let sortedId: string;
let previewsId: string;
let trashedId: string;
/** File ids in the preview folder, by name — `?preview=` is keyed on them. */
const previewIds = new Map<string, string>();

test.describe.configure({ mode: 'serial' });

// Six real uploads and a sign-in; the default 30s is not the budget for that.
test.beforeEach(() => test.setTimeout(120_000));

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  page = await browser.newPage();
  store = await playwrightRequest.newContext();

  // The root id comes from the API; the cookie it sets is dropped again so the
  // login screen is genuinely exercised below.
  const login = await page.request.post(`${BASE}/api/auth/login`, { headers: CLIENT, data: ACCOUNT });
  expect(login.status(), 'the seeded account signs in — did `make e2e` seed this stack?').toBe(200);
  const rootId = (await login.json()).root_id as string;

  sortedId = await createFolder(rootId, SORTED);
  const fullId = await createFolder(sortedId, FULL_FOLDER);
  await createFolder(fullId, 'Note 1');
  await createFolder(fullId, 'Note 2');
  await createFolder(sortedId, EMPTY_FOLDER);

  previewsId = await createFolder(rootId, PREVIEWS);
  await createFolder(previewsId, MIDDLE_FOLDER);

  trashedId = await createFolder(rootId, TRASHED);
  for (const box of BOXES) await createFolder(trashedId, box);

  await page.context().clearCookies();
  await signIn(page);

  // One at a time, each awaited: `updated_at` is the publish time, so a
  // sequential upload is the only way to know what the newest-first order is.
  await open(sortedId, SORTED);
  for (const file of SORT_FILES) {
    await upload([{ name: file.name, mimeType: 'text/plain', buffer: filler(file.name, file.size) }]);
    await expect(rowNamed(file.name)).toBeVisible({ timeout: 60_000 });
  }

  await open(previewsId, PREVIEWS);
  await upload(PREVIEW_FILES);
  for (const file of PREVIEW_FILES) {
    await expect(rowNamed(file.name)).toBeVisible({ timeout: 60_000 });
  }
  for (const node of await childrenOf(previewsId)) {
    if (node.kind === 'file') previewIds.set(node.name, node.id);
  }
  expect(previewIds.size, 'every preview fixture landed').toBe(PREVIEW_FILES.length);

  // The dock parks in the bottom-right corner once it has something to report
  // and would sit over the rows the cases click.
  const clear = page.getByRole('button', { name: 'Clear finished' });
  if (await clear.isVisible()) {
    await clear.click();
    await expect(page.getByRole('complementary', { name: 'Uploads' })).toBeHidden();
  }
});

test.afterAll(async () => {
  await store?.dispose();
  await page?.close();
});

/* ---------------------------------------------------------------- sorting */

test('the fixture has three different orders, so each sort is a claim', () => {
  // The guard that keeps the cases below from passing by accident: if the size
  // order or the upload order happened to equal the name order, a server that
  // ignored `?sort` entirely would satisfy them.
  expect(BY_SIZE_ASC).not.toEqual(BY_NAME);
  expect(BY_UPDATED_DESC).not.toEqual(BY_NAME);
  expect(BY_UPDATED_DESC).not.toEqual(BY_SIZE_ASC);
  expect(new Set(SORT_FILES.map((f) => f.size)).size).toBe(SORT_FILES.length);
});

test('clicking Size sorts by size, both ways, with folders always first', async () => {
  await open(sortedId, SORTED);

  // The folder opens on the server's default, which is name ascending.
  await expect.poll(fileNames).toEqual(BY_NAME);
  expect(new URL(page.url()).searchParams.get('sort'), 'nothing in the URL yet').toBeNull();

  await page.getByRole('button', { name: 'Size', exact: true }).click();

  const ascending = new URL(page.url()).searchParams;
  expect(ascending.get('sort')).toBe('size');
  expect(ascending.get('dir')).toBe('asc');
  await expect.poll(fileNames).toEqual(BY_SIZE_ASC);
  await expectFoldersFirst();

  await page.getByRole('button', { name: /^Size\b/ }).click();

  const descending = new URL(page.url()).searchParams;
  expect(descending.get('sort')).toBe('size');
  expect(descending.get('dir')).toBe('desc');
  await expect.poll(fileNames).toEqual(BY_SIZE_DESC);
  // The half of the contract a descending sort is the only test of: folders
  // rank above files whichever way the key runs. Drop the rank from the
  // server's ORDER BY and an ascending size sort still puts them first — they
  // have no size — while this line fails.
  await expectFoldersFirst();
});

test('a sorted folder is a place: the first request off a reload carries it', async () => {
  const first = page.waitForRequest(
    (request) => request.url().includes(`/api/nodes/${sortedId}/children`),
  );

  await page.goto(`${BASE}/folders/${sortedId}?sort=updated_at&dir=desc`);

  // Not "a request carries it" — the FIRST one. A screen that read the URL
  // only after an unsorted first page would still end up sorted, having shown
  // the wrong rows and asked the server twice.
  const params = new URL((await first).url()).searchParams;
  expect(params.get('sort')).toBe('updated_at');
  expect(params.get('dir')).toBe('desc');

  await expect.poll(fileNames).toEqual(BY_UPDATED_DESC);
  await expectFoldersFirst();
});

test('a folder row says how much it holds', async () => {
  await open(sortedId, SORTED);

  await expect(rowNamed(FULL_FOLDER)).toContainText('2 items');
  // The one that says the count is counted rather than defaulted: an empty
  // folder has to say "0 items", not go blank.
  await expect(rowNamed(EMPTY_FOLDER)).toContainText('0 items');
  // And a file quotes bytes in that same cell, never a count.
  await expect(rowNamed('pear.txt')).not.toContainText('items');
});

/* --------------------------------------------------------------- previews */

test('the file name opens the viewer, and the arrows walk the files', async () => {
  await open(previewsId, PREVIEWS);

  const png = await openByName('apple.png');
  expect(new URL(page.url()).searchParams.get('preview')).toBe(png);

  // An image is pointed at the store, never proxied: the src is somewhere
  // else entirely, signed, and carries the type the server chose to serve it
  // as — the allowlist's own constant, not the row's stored MIME.
  const image = dialog().getByRole('img', { name: 'apple.png' });
  const src = await srcOf(image);
  expect(src.origin, 'the bytes do not come from the app').not.toBe(APP_ORIGIN);
  expect(src.searchParams.get('X-Amz-Signature'), 'the link is presigned').toBeTruthy();
  expect(src.searchParams.get('response-content-type')).toBe('image/png');
  const served = await store.get(src.toString());
  expect(served.status()).toBe(200);
  expect(served.headers()['content-type']).toBe('image/png');

  await page.keyboard.press('ArrowRight');
  await expect(dialog().getByText('banana.txt')).toBeVisible();
  // Text is read with a bare fetch off the store and shown as itself.
  await expect(dialog().locator('pre')).toContainText('the file the viewer reads straight off the store');

  await page.keyboard.press('ArrowRight');
  await expect(dialog().getByText('date.pdf')).toBeVisible();
  const frame = dialog().locator('iframe');
  // Deliberately attribute-free: a sandboxed cross-origin frame gets the
  // broken-document placeholder from Chromium's built-in viewer, so the
  // sandbox would buy a frame nobody can read. What holds the line is the
  // origin and the forced content type, both asserted here.
  expect(await frame.getAttribute('sandbox'), 'the PDF frame is not sandboxed').toBeNull();
  const pdf = await srcOf(frame);
  expect(pdf.origin).not.toBe(APP_ORIGIN);
  const pdfServed = await store.get(pdf.toString());
  expect(pdfServed.status()).toBe(200);
  expect(pdfServed.headers()['content-type']).toBe('application/pdf');

  // Three steps, three files, and never the folder sitting between two of
  // them by name: the counter counts files, not rows.
  await expect(dialog().getByText(`3 of ${PREVIEW_FILES.length}`)).toBeVisible();
  await expect(rowNamed(MIDDLE_FOLDER)).toBeVisible();
  await expect(dialog().getByText(MIDDLE_FOLDER)).toHaveCount(0);
});

test('Escape closes the viewer and hands focus back to the name that opened it', async () => {
  await open(previewsId, PREVIEWS);
  const png = await openByName('apple.png');

  await page.keyboard.press('Escape');

  await expect(dialog()).toHaveCount(0);
  expect(new URL(page.url()).searchParams.get('preview'), 'the parameter is gone').toBeNull();
  // Radix hands focus back to the trigger, and this dialog has none — it was
  // opened by following a link. Landing anywhere else is a lost place in the
  // list.
  await expect(page.locator(`[data-preview-id="${png}"]`)).toBeFocused();
});

test("the kebab's Preview opens the same viewer as the name", async () => {
  await open(previewsId, PREVIEWS);

  await rowNamed('apple.png').getByRole('button', { name: 'Actions for apple.png' }).click();
  await page.getByRole('menuitem', { name: 'Preview' }).click();

  expect(new URL(page.url()).searchParams.get('preview')).toBe(previewIds.get('apple.png'));
  await expect(dialog().getByRole('img', { name: 'apple.png' })).toBeVisible();

  await page.keyboard.press('Escape');
  await expect(dialog()).toHaveCount(0);
});

test('clicking the size cell selects the row rather than opening it', async () => {
  await open(previewsId, PREVIEWS);

  const row = rowNamed('apple.png');
  // The trailing cell, which is not a link and not a control — so the row's
  // own click handler answers for it. One affordance opens a file, and it is
  // the name.
  const sizeCell = row.locator('span.numeric').last();
  await expect(sizeCell).toHaveText(/\d/);
  await sizeCell.click();

  await expect(row).toHaveAttribute('data-selected', '');
  expect(new URL(page.url()).searchParams.get('preview')).toBeNull();
  await expect(dialog()).toHaveCount(0);

  await page.keyboard.press('Escape');
});

test('a type the server will not sign for is answered without touching the store', async () => {
  await open(previewsId, PREVIEWS);

  // The control first. Without it, "no request to the store" is also what a
  // viewer that never asks the store for anything would say.
  const forImage = await requestsAway(async () => {
    await openByName('apple.png');
    await expect(dialog().getByRole('img', { name: 'apple.png' })).toBeVisible();
    await settle();
  });
  expect(forImage.length, 'an image really does come off the store').toBeGreaterThan(0);
  await page.keyboard.press('Escape');
  await expect(dialog()).toHaveCount(0);

  for (const name of ['elder.svg', 'sketch.bmp']) {
    const away = await requestsAway(async () => {
      await openByName(name);
      await expect(dialog().getByTestId('no-preview')).toBeVisible();
      await settle();
    });

    await expect(dialog().getByText('No preview for this type')).toBeVisible();
    // The honest offer that replaces the picture.
    const download = dialog().getByTestId('no-preview').getByRole('link', { name: 'Download' });
    await expect(download).toHaveAttribute('href', `/api/files/${previewIds.get(name)}/download`);
    // The claim: the app never went and fetched bytes it had been told it may
    // not show. A server that echoed the stored MIME back instead of the
    // allowlist's constant would sign `image/bmp`, the viewer would put it in
    // an `<img>`, and this line would fail.
    expect(away, `nothing was fetched off the store for ${name}`).toEqual([]);

    await page.keyboard.press('Escape');
    await expect(dialog()).toHaveCount(0);
  }
});

test('a preview opens and closes inside a sorted folder without losing the sort', async () => {
  const bySize = [...PREVIEW_FILES].sort((a, b) => a.buffer.length - b.buffer.length).map((f) => f.name);
  const byName = [...PREVIEW_FILES].map((f) => f.name).sort();
  // Same guard as the sort fixture: if these two agreed, the case would pass
  // against a server that ignored the parameter.
  expect(bySize).not.toEqual(byName);
  expect(new Set(PREVIEW_FILES.map((f) => f.buffer.length)).size).toBe(PREVIEW_FILES.length);

  await page.goto(`${BASE}/folders/${previewsId}?sort=size&dir=asc`);
  await expect.poll(fileNames).toEqual(bySize);

  await openByName('apple.png');
  const opened = new URL(page.url()).searchParams;
  expect(opened.get('sort'), 'the viewer rides the sort rather than replacing it').toBe('size');
  expect(opened.get('dir')).toBe('asc');

  await page.keyboard.press('Escape');
  await expect(dialog()).toHaveCount(0);

  const closed = new URL(page.url()).searchParams;
  expect(closed.get('preview')).toBeNull();
  expect(closed.get('sort')).toBe('size');
  expect(closed.get('dir')).toBe('asc');
  await expect.poll(fileNames).toEqual(bySize);
});

/* ------------------------------------------------------------------ trash */

test('a selection goes to the trash and comes back out of it whole', async () => {
  await open(trashedId, TRASHED);
  await expect(rows()).toHaveCount(BOXES.length);

  await selectRows(['Box A', 'Box B', 'Box C']);
  await band().getByRole('button', { name: 'Trash' }).click();
  await expect(rows()).toHaveCount(1);

  await openTrash();
  await expect(rows()).toHaveCount(3);
  // The date column here is `deleted_at`, which the trash DTO is the only
  // listing to carry: without it the cell renders nothing and has no title at
  // all.
  // A plain label, not a sort control: the trash listing has no sort, so
  // `FileList` renders its column headings as text.
  await expect(page.getByText('Trashed', { exact: true })).toBeVisible();
  const when = await trashRow('Box A').locator('span.numeric').first().getAttribute('title');
  expect(when, 'the Trashed column is populated').toBeTruthy();
  expect(Number.isFinite(new Date(when!).getTime()), `"${when}" is a time`).toBe(true);

  await page.getByTestId('select-all').click();
  await expect(band().getByText('3 selected')).toBeVisible();
  await band().getByRole('button', { name: 'Restore' }).click();

  await expect(page.getByText('The trash is empty.')).toBeVisible();
  await open(trashedId, TRASHED);
  await expect(rows()).toHaveCount(BOXES.length);
  for (const box of BOXES) await expect(rowNamed(box)).toBeVisible();
});

test('Delete forever takes the selection and leaves the rest', async () => {
  await open(trashedId, TRASHED);
  await selectRows(['Box A', 'Box B', 'Box C']);
  await band().getByRole('button', { name: 'Trash' }).click();
  await expect(rows()).toHaveCount(1);

  await openTrash();
  await expect(rows()).toHaveCount(3);

  await selectRows(['Box A', 'Box B']);
  await expect(band().getByText('2 selected')).toBeVisible();
  await band().getByRole('button', { name: 'Delete forever' }).click();

  await expect(rows()).toHaveCount(1);
  await expect(trashRow('Box C')).toBeVisible();
  // Gone for good, not merely out of the listing.
  await open(trashedId, TRASHED);
  await expect(rowNamed('Box A')).toHaveCount(0);
  await expect(rowNamed('Box B')).toHaveCount(0);
});

test('right-clicking a trashed row restores it from the menu', async () => {
  await openTrash();
  await expect(rows()).toHaveCount(1);

  await trashRow('Box C').click({ button: 'right' });
  const menu = page.locator('[data-slot="context-menu-content"]');
  await expect(menu).toBeVisible();
  await menu.getByRole('menuitem', { name: 'Restore' }).click();

  await expect(page.getByText('The trash is empty.')).toBeVisible();
  await open(trashedId, TRASHED);
  await expect(rowNamed('Box C')).toBeVisible();
});

test('Empty trash names the count, and then the trash is empty', async () => {
  await open(trashedId, TRASHED);
  await selectRows(['Box C', 'Box D']);
  await band().getByRole('button', { name: 'Trash' }).click();
  // Both rows gone from the folder before leaving it: the band spends a
  // selection as one command per row, and navigating on the click alone races
  // the listing that is about to be asked for.
  await expect(rows()).toHaveCount(0);

  await openTrash();
  await expect(rows()).toHaveCount(2);

  await band().getByRole('button', { name: 'Empty trash' }).click();
  const confirm = page.getByRole('dialog');
  // The one confirmation in the app that cannot be walked back says the number
  // out loud, and it is the number of rows it is about to take.
  await expect(confirm.getByRole('heading', { name: 'Delete all 2 items forever?' })).toBeVisible();
  await confirm.getByRole('button', { name: 'Empty trash' }).click();

  await expect(page.getByText('The trash is empty.')).toBeVisible();
  await expect(rows()).toHaveCount(0);
});

/* ---------------------------------------------------------------- helpers */

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

async function createFolder(parentId: string, name: string): Promise<string> {
  const res = await page.request.post(`${BASE}/api/folders`, {
    headers: CLIENT,
    data: { parent_id: parentId, name },
  });
  expect(res.status(), `created ${name}`).toBe(201);
  return (await res.json()).id as string;
}

interface ApiNode {
  id: string;
  name: string;
  kind: 'file' | 'folder';
}

async function childrenOf(folderId: string): Promise<ApiNode[]> {
  const res = await page.request.get(`${BASE}/api/nodes/${folderId}/children`);
  expect(res.status()).toBe(200);
  return (await res.json()).items as ApiNode[];
}

async function signIn(target: Page): Promise<void> {
  await target.goto(`${BASE}/login`);
  await target.getByLabel('Email').fill(ACCOUNT.email);
  await target.getByLabel('Password').fill(ACCOUNT.password);
  await target.getByRole('button', { name: 'Sign in' }).click();
  await expect(target.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();
}

/** Back to a fixture folder with nothing selected, whatever the last case left. */
async function open(folderId: string, name: string): Promise<void> {
  await page.goto(`${BASE}/folders/${folderId}`);
  await expect(
    page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]'),
  ).toHaveText(name);
  await expect(list()).toBeVisible();
}

async function openTrash(): Promise<void> {
  await page.goto(`${BASE}/trash`);
  await expect(page.getByRole('heading', { name: 'Trash' })).toBeVisible();
}

/** The hidden input the New menu drives. `setInputFiles` reaches it directly. */
async function upload(files: Fixture[]): Promise<void> {
  await page.getByLabel('Upload files').setInputFiles(files);
}

/** Clicking a file's name — the one affordance that opens it. Returns its id. */
async function openByName(name: string): Promise<string> {
  const id = previewIds.get(name);
  expect(id, `${name} is a known fixture`).toBeTruthy();
  await rowNamed(name).getByRole('link', { name }).click();
  await expect(dialog()).toBeVisible();
  return id!;
}

/** Selects exactly these rows through their own checkboxes. */
async function selectRows(names: string[]): Promise<void> {
  for (const name of names) {
    await rows().filter({ hasText: name }).getByRole('checkbox').click();
  }
}

/** The rendered order of the file rows — folders have their own assertion. */
async function fileNames(): Promise<string[]> {
  const names = (await page.locator('[data-testid="file-row"] a').allTextContents()).map((t) => t.trim());
  return names.filter((name) => name.includes('.'));
}

/**
 * Folders rank above files whichever way the key runs — the server's rule, and
 * the one an ORDER BY that let the rank follow `dir` would break.
 */
async function expectFoldersFirst(): Promise<void> {
  const names = (await page.locator('[data-testid="file-row"] a').allTextContents()).map((t) => t.trim());
  const folders = names.filter((name) => !name.includes('.'));
  expect(names.slice(0, folders.length)).toEqual(folders);
  expect(folders.length, 'there are folders to be first').toBeGreaterThan(0);
}

async function srcOf(locator: Locator): Promise<URL> {
  const src = await locator.getAttribute('src');
  expect(src, 'the element points at something').toBeTruthy();
  return new URL(src!);
}

/**
 * Every request the page made to somewhere other than the app while `run` ran.
 * The object store is a different origin, so anything in here is bytes the
 * viewer went and fetched.
 */
async function requestsAway(run: () => Promise<void>): Promise<string[]> {
  const away: string[] = [];
  const record = (url: string) => {
    if (!/^https?:/.test(url)) return;
    if (new URL(url).origin !== APP_ORIGIN) away.push(url);
  };
  const listener = (request: { url: () => string }) => record(request.url());
  page.on('request', listener);
  try {
    await run();
  } finally {
    page.off('request', listener);
  }
  return away;
}

/**
 * A beat after the screen has settled, because "no request was made" is a claim
 * about a request that would arrive slightly after the element it belongs to.
 */
async function settle(): Promise<void> {
  await page.waitForTimeout(500);
}

const list = () => page.locator('[data-testid="file-list"]');
const rows = () => page.locator('[data-testid="file-row"]');
const rowNamed = (name: string) => rows().filter({ hasText: name });
/** Trash rows have no name links, so they are found by their text. */
const trashRow = (name: string) => rows().filter({ hasText: name });
const band = () => page.getByTestId('command-band');
const dialog = () => page.getByRole('dialog');
