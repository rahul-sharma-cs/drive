import { expect, test, type Page, type Request } from '@playwright/test';
import { createHash, randomUUID } from 'node:crypto';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The zip download, in a real browser — the only place it can be proved.
 *
 * The archive is built in the tab: the app walks the folders through the API,
 * asks for one presigned link per file, fetches the bytes straight off the
 * object store and feeds them to client-zip. There is no server-side archive
 * and there is not going to be one, so nothing on the Go side can be asked
 * whether the zip is right. The vitest suite proves the walk and the failure
 * handling against stubs; what it cannot do is produce a file. This spec ends
 * with a real .zip on disk, opens it, and compares the bytes inside it against
 * the bytes that were uploaded.
 *
 * **Chromium is deliberately crippled here.** It is the one engine with
 * `showSaveFilePicker`, and Playwright cannot answer a native save dialog — so
 * an init script deletes the function on every page in this file, which puts
 * the app on the Blob path (`blobSink`) that Safari and Firefox take anyway.
 * That is the path this spec covers end to end. The streaming path — the
 * picker, the disk handle, an archive bigger than memory — stays a manual
 * check, and is called out in the report rather than pretended at.
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
 * owns the sample tree the Go and search tests read.
 */
const ACCOUNT = { email: 'demo@drive.local', password: 'drive-demo-1' };

const RUN = randomUUID().slice(0, 8);

const CLIENT = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

/* ------------------------------------------------------------------ fixture */

/**
 * One folder holding everything the archive has to get right:
 *
 *   Zip <run>/
 *     Empty/                 an empty folder, which a zip loses unless it is
 *                            written as a directory entry of its own
 *     Notes/
 *       Deep/
 *         deep.bin           two levels down, and binary
 *       inner.txt
 *     second.txt
 *     top.txt
 *
 * Four files, four different contents, four different lengths — see the guard
 * case below, which is what stops "byte-for-byte equal" from being satisfiable
 * by an app that handed every entry the same object.
 */
const FIXTURE = `Zip ${RUN}`;
const NOTES = 'Notes';
const DEEP = 'Deep';
const EMPTY = 'Empty';

const CONTENT: Record<string, Buffer> = {
  'top.txt': Buffer.from('the file at the top of the fixture\n'.repeat(3), 'utf8'),
  'second.txt': Buffer.from('the second file, so a selection can hold two\n'.repeat(5), 'utf8'),
  'inner.txt': Buffer.from('one level down, inside the Notes folder\n'.repeat(7), 'utf8'),
  // Binary on purpose: a byte-for-byte claim that only ever saw UTF-8 text
  // would pass against a pipeline that decoded and re-encoded everything.
  'deep.bin': Buffer.from(Uint8Array.from({ length: 512 }, (_, i) => (i * 7 + 13) % 256)),
};

/** Where each file ends up inside an archive of the whole fixture folder. */
const IN_ARCHIVE: Record<string, string> = {
  'top.txt': `${FIXTURE}/top.txt`,
  'second.txt': `${FIXTURE}/second.txt`,
  'inner.txt': `${FIXTURE}/${NOTES}/inner.txt`,
  'deep.bin': `${FIXTURE}/${NOTES}/${DEEP}/deep.bin`,
};

const EMPTY_IN_ARCHIVE = `${FIXTURE}/${EMPTY}/`;

/**
 * `failureMessage`'s wording in `useZipDownload`, typographic punctuation and
 * all. It names the archive as well as the file that sank it: on the streaming
 * path the browser created that file the moment the save dialog was answered,
 * so a person has to be able to tell which file on their disk is the dud.
 */
const REFUSED_TOAST = `Couldn’t download “inner.txt” — “${FIXTURE}.zip” was not saved`;

/** One signed-in page for the whole file: signing in once per case proves nothing extra. */
let page: Page;
let rootId: string;
let fixtureId: string;
/** File ids in the fixture folder, by name — the single-file Download links at them. */
const fileIds = new Map<string, string>();
/** Somewhere for `download.saveAs` to put the archives this spec opens. */
let saved: string;

test.describe.configure({ mode: 'serial' });

// Four uploads, four folder creations and a sign-in; the default 30s is not the
// budget for that.
test.beforeEach(() => test.setTimeout(120_000));

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  saved = mkdtempSync(join(tmpdir(), 'drive-zip-'));
  page = await browser.newPage();

  // Before anything is loaded, and for every document this page opens: with the
  // picker gone `canStreamToDisk()` is false and the app assembles the archive
  // in memory and hands it over as a Blob — the path every non-Chromium browser
  // is on, and the only one a driver can follow.
  await page.addInitScript(() => {
    Reflect.deleteProperty(window, 'showSaveFilePicker');
    if (typeof (window as { showSaveFilePicker?: unknown }).showSaveFilePicker === 'function') {
      Object.defineProperty(window, 'showSaveFilePicker', { configurable: true, value: undefined });
    }
  });

  // The root id comes from the API; the cookie it sets is dropped again so the
  // login screen is genuinely exercised below.
  const login = await page.request.post(`${BASE}/api/auth/login`, { headers: CLIENT, data: ACCOUNT });
  expect(login.status(), 'the seeded account signs in — did `make e2e` seed this stack?').toBe(200);
  rootId = (await login.json()).root_id as string;

  // Only the fixture's own folder is made through the API, because its id is
  // what everything below navigates by. The three that the archive's shape
  // depends on are made the way a person makes them, through `+ New`.
  fixtureId = await createFolder(rootId, FIXTURE);

  await page.context().clearCookies();
  await signIn(page);

  expect(
    await page.evaluate(() => typeof (window as { showSaveFilePicker?: unknown }).showSaveFilePicker),
    'the save picker is out of the way, so the app is on the Blob path',
  ).toBe('undefined');

  await open(fixtureId, FIXTURE);
  await newFolder(EMPTY);
  await newFolder(NOTES);
  await upload(['top.txt', 'second.txt']);

  await openByName(NOTES);
  await newFolder(DEEP);
  await upload(['inner.txt']);

  await openByName(DEEP);
  await upload(['deep.bin']);

  for (const node of await childrenOf(fixtureId)) {
    if (node.kind === 'file') fileIds.set(node.name, node.id);
  }
  expect(fileIds.size, 'both top-level fixture files landed').toBe(2);

  // The upload dock parks in the bottom-right corner once it has something to
  // report, which is where the archive dock goes too.
  await clearDock();
});

test.afterAll(async () => {
  await page?.close();
});

/* -------------------------------------------------------------------- guard */

test('the fixture is four different files, so "byte-for-byte" is a claim', () => {
  const names = Object.keys(CONTENT);
  expect(new Set(names.map((n) => sha256(CONTENT[n]))).size, 'four distinct contents').toBe(names.length);
  expect(new Set(names.map((n) => CONTENT[n].length)).size, 'four distinct lengths').toBe(names.length);
  // And they are nested at four different depths of path, so a walk that
  // flattened or truncated the tree could not land on the right answer either.
  expect(new Set(Object.values(IN_ARCHIVE)).size).toBe(names.length);
});

/* ---------------------------------------------------------------- folder zip */

test('a folder downloads as one zip holding everything under it', async () => {
  await open(rootId, 'My Drive');
  await select(FIXTURE);

  const archive = await archiveFrom(() => band().getByRole('button', { name: 'Download' }).click());

  // Named after the folder, because one folder is a thing with a name.
  expect(archive.name).toBe(`${FIXTURE}.zip`);

  const paths = archive.entries.map((entry) => entry.path).sort();
  expect(paths, 'every file, at its nested path, plus the empty folder').toEqual(
    [...Object.values(IN_ARCHIVE), EMPTY_IN_ARCHIVE].sort(),
  );

  for (const [name, expected] of Object.entries(CONTENT)) {
    const entry = archive.entries.find((e) => e.path === IN_ARCHIVE[name]);
    expect(entry, `${IN_ARCHIVE[name]} is in the archive`).toBeDefined();
    expect(entry!.size, `${name} is its own length`).toBe(expected.length);
    // The whole point of the case: what came out of the archive is what went
    // in, byte for byte, two levels of nesting and 512 binary bytes included.
    expect(sha256(entry!.bytes), `${name} hashes equal to what was uploaded`).toBe(sha256(expected));
  }

  // An empty folder has no bytes to carry it, so it survives only as a
  // directory entry of its own — drop that and the folder is gone from the zip.
  const empty = archive.entries.find((e) => e.path === EMPTY_IN_ARCHIVE);
  expect(empty, 'the empty folder is in the archive').toBeDefined();
  expect(empty!.isDirectory, 'and is written as a directory entry').toBe(true);
  expect(empty!.size).toBe(0);
});

/* ----------------------------------------------------------- multi-selection */

test('two selected files download as one dated archive', async () => {
  await open(fixtureId, FIXTURE);
  await select('second.txt');
  await select('top.txt');
  await expect(band().getByText('2 selected')).toBeVisible();

  const archive = await archiveFrom(() => band().getByRole('button', { name: 'Download' }).click());

  // A set of things has no name but the moment it was asked for.
  expect(archive.name, 'named for when it was asked for').toMatch(/^drive-\d{8}-\d{4}\.zip$/);

  // Exactly those two, at the top of the archive: a selection is not a folder,
  // so nothing wraps them and nothing else comes along.
  expect(archive.entries.map((e) => e.path).sort()).toEqual(['second.txt', 'top.txt']);
  for (const name of ['second.txt', 'top.txt']) {
    const entry = archive.entries.find((e) => e.path === name)!;
    expect(sha256(entry.bytes), `${name} hashes equal to what was uploaded`).toBe(sha256(CONTENT[name]));
  }
});

/* -------------------------------------------------------------- single file */

test('one file on its own is still a plain download, not an archive', async () => {
  await open(fixtureId, FIXTURE);
  await select('top.txt');

  // A link, not a command: one file has a URL of its own, and going to it hands
  // the transfer to the browser — its progress, its resume, its right-click
  // menu. Wrapping it in a zip built here would take all of that away.
  const download = band().getByRole('link', { name: 'Download' });
  await expect(download).toHaveAttribute('href', `/api/files/${fileIds.get('top.txt')}/download`);
  await expect(band().getByRole('button', { name: 'Download' }), 'nothing here builds an archive').toHaveCount(0);
  await expect(dock()).toBeHidden();
});

/* -------------------------------------------------------------- the kebab */

test('the row menu downloads a folder as a zip too', async () => {
  await open(rootId, 'My Drive');
  await rowNamed(FIXTURE).getByRole('button', { name: `Actions for ${FIXTURE}` }).click();

  const archive = await archiveFrom(() => page.getByRole('menuitem', { name: 'Download' }).click());

  // The same archive the band builds, from a row that was never selected: both
  // affordances end in the same call, which is the claim.
  expect(archive.name).toBe(`${FIXTURE}.zip`);
  expect(archive.entries.map((e) => e.path).sort()).toEqual(
    [...Object.values(IN_ARCHIVE), EMPTY_IN_ARCHIVE].sort(),
  );
});

/* ------------------------------------------------------------- the network */

test('the bytes are fetched off the store with no app header and no cookie', async () => {
  await open(fixtureId, FIXTURE);
  await select(NOTES);

  const away: Request[] = [];
  const watch = (request: Request) => {
    if (/^https?:/.test(request.url()) && new URL(request.url()).origin !== APP_ORIGIN) away.push(request);
  };
  page.on('request', watch);
  try {
    const archive = await archiveFrom(() => band().getByRole('button', { name: 'Download' }).click());
    expect(archive.entries.map((e) => e.path).sort()).toEqual([`${NOTES}/${DEEP}/deep.bin`, `${NOTES}/inner.txt`]);
  } finally {
    page.off('request', watch);
  }

  // The control: without it, every assertion below is also what a browser that
  // never went to the store would say.
  expect(away.length, 'the archive really was filled from the object store').toBeGreaterThan(0);

  for (const request of away) {
    const headers = await request.allHeaders();
    // `request()` in lib/api adds X-Drive-Client to everything it sends, and a
    // custom header on a cross-origin GET turns it into a preflight the store's
    // enumerated CORS rule answers with a 403. So the bytes are fetched with a
    // bare `fetch`, and this is the assertion that keeps it that way.
    expect(headers['x-drive-client'], `${request.method()} ${request.url()} carries no client header`).toBeUndefined();
    // And the store is not the app: an app cookie has no business going there.
    expect(headers['cookie'], 'no session cookie rides along to the store').toBeUndefined();
    // The preflight itself, which is what the header would have caused.
    expect(request.method(), 'no preflight was needed').toBe('GET');
  }
});

/* ----------------------------------------------------------------- cancel */

test('cancelling mid-archive saves nothing, and the next archive still works', async () => {
  await open(rootId, 'My Drive');
  await select(FIXTURE);

  // One entry's bytes are held up long enough that Cancel lands in the middle
  // of the archive rather than after it. `deep.bin` is the first file the walk
  // reaches, so the stall is at the start with everything still to come.
  // The matcher is held in a variable because `unroute` finds a route by the
  // identity of the function it was registered with.
  const slow = storeGetFor('deep.bin');
  await page.route(slow, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 8_000));
    // By the time this wakes up the cancel has already taken the request away,
    // and continuing a request that no longer exists is not an interesting
    // failure — it is the thing the case just proved.
    await route.continue().catch(() => {});
  });

  try {
    await expectNoArchive(async () => {
      await band().getByRole('button', { name: 'Download' }).click();
      await expect(dock()).toBeVisible();
      await expect(dock().getByText(`${FIXTURE}.zip`)).toBeVisible();
      await dock().getByRole('button', { name: 'Cancel' }).click();
      // Gone, rather than left sitting at a percentage that will never move:
      // the walk, the fetch in flight and the writer all stop together.
      await expect(dock()).toBeHidden();
    });
  } finally {
    await page.unroute(slow);
  }

  // And the app is not stuck holding a cancelled job: the one-at-a-time guard
  // turns away a second archive while one is running, so a cancel that failed
  // to clear it would refuse every archive for the life of the tab.
  await open(rootId, 'My Drive');
  await select(FIXTURE);
  const archive = await archiveFrom(() => band().getByRole('button', { name: 'Download' }).click());
  expect(archive.name).toBe(`${FIXTURE}.zip`);
  expect(archive.entries.length).toBe(Object.keys(CONTENT).length + 1);
});

/* ------------------------------------------------------------ a failed entry */

test('a file the store will not hand over aborts the whole archive', async () => {
  await open(rootId, 'My Drive');
  await select(FIXTURE);

  // 403 from the store, with the CORS header the browser needs in order to
  // *read* the 403 — without it the fetch is a network error instead, which is
  // a different branch of the app than the one under test here. The matcher is
  // held because `unroute` finds a route by the identity of its function.
  const refused = storeGetFor('inner.txt');
  await page.route(refused, (route) =>
    route.fulfill({
      status: 403,
      headers: { 'access-control-allow-origin': APP_ORIGIN, 'content-type': 'application/xml' },
      body: '<Error><Code>AccessDenied</Code></Error>',
    }),
  );

  try {
    await expectNoArchive(async () => {
      await band().getByRole('button', { name: 'Download' }).click();
      // Names the file, because "something went wrong" would leave a person
      // with a folder of hundreds and no idea which one to look at — and names
      // the archive it sank, which is the file that is not on their disk.
      await expect(
        toasts().filter({ hasText: REFUSED_TOAST }).first(),
        'the toast names the file that could not be fetched, and the archive it sank',
      ).toBeVisible();
      await expect(dock()).toBeHidden();
    });
  } finally {
    await page.unroute(refused);
  }

  // Half an archive with a name on it is worse than none — it looks like the
  // folder, and it is missing whatever came after the failure. So: nothing was
  // written, which `expectNoArchive` has just asserted, and the dock is clear.
  await expect(dock()).toBeHidden();
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

const sha256 = (bytes: Buffer | Uint8Array): string => createHash('sha256').update(bytes).digest('hex');

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

/** Back to a folder with nothing selected, whatever the last case left behind. */
async function open(folderId: string, name: string): Promise<void> {
  await page.goto(`${BASE}/folders/${folderId}`);
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]')).toHaveText(name);
  // Either shape of a settled folder screen: the list, or the empty state that
  // replaces it. The fixture folder starts out empty and is filled below.
  await expect(list().or(page.getByText('This folder is empty.'))).toBeVisible();
}

/** Into a subfolder by clicking its name, the way a person gets there. */
async function openByName(name: string): Promise<void> {
  await rowNamed(name).getByRole('link', { name }).click();
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]')).toHaveText(name);
}

/** `+ New` → New folder, in the folder on screen. */
async function newFolder(name: string): Promise<void> {
  await page.getByRole('button', { name: 'New' }).click();
  await page.getByRole('menuitem', { name: 'New folder' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog.getByRole('heading', { name: 'New folder' })).toBeVisible();
  await dialog.getByLabel('Name').fill(name);
  await dialog.getByRole('button', { name: 'Create' }).click();
  await expect(rowNamed(name)).toBeVisible();
}

/** Real uploads through the hidden input the New menu drives, into the folder on screen. */
async function upload(names: string[]): Promise<void> {
  await page.getByLabel('Upload files').setInputFiles(
    names.map((name) => ({ name, mimeType: mimeOf(name), buffer: CONTENT[name] })),
  );
  for (const name of names) await expect(rowNamed(name)).toBeVisible({ timeout: 60_000 });
}

const mimeOf = (name: string): string => (name.endsWith('.txt') ? 'text/plain' : 'application/octet-stream');

/** Adds one row to the selection through its own checkbox. */
async function select(name: string): Promise<void> {
  await rowNamed(name).getByRole('checkbox').click();
  await expect(rowNamed(name)).toHaveAttribute('data-selected', '');
}

/** Clears the upload dock if it is reporting, so it is not over the rows. */
async function clearDock(): Promise<void> {
  const clear = page.getByRole('button', { name: 'Clear finished' });
  if (await clear.isVisible()) {
    await clear.click();
    await expect(page.getByRole('complementary', { name: 'Uploads' })).toBeHidden();
  }
}

interface ArchiveEntry {
  path: string;
  /** Uncompressed length, from the central directory. */
  size: number;
  /** The entry's bytes, sliced out of the archive. */
  bytes: Buffer;
  isDirectory: boolean;
}

/**
 * Runs `start`, waits for the download it causes, and opens the archive.
 *
 * The download arrives as a Blob the app named and clicked an anchor at, which
 * is what `blobSink` does everywhere the File System Access API is missing —
 * and, here, everywhere, because the init script took the picker away.
 */
async function archiveFrom(start: () => Promise<void>): Promise<{ name: string; entries: ArchiveEntry[] }> {
  const arriving = page.waitForEvent('download', { timeout: 60_000 });
  await start();
  const download = await arriving;
  const to = join(saved, `${randomUUID()}.zip`);
  await download.saveAs(to);
  // The dock is the app saying it is still working; the archive is on disk, so
  // it has to be gone before the next case starts.
  await expect(dock()).toBeHidden();
  return { name: download.suggestedFilename(), entries: readArchive(readFileSync(to)) };
}

/** `run`, and then a beat: nothing the browser would call a download happened. */
async function expectNoArchive(run: () => Promise<void>): Promise<void> {
  let downloads = 0;
  const count = () => {
    downloads += 1;
  };
  page.on('download', count);
  try {
    await run();
    // "No download" is a claim about one that would arrive slightly after the
    // screen settled, so the wait is part of the assertion rather than padding.
    await page.waitForTimeout(2_000);
  } finally {
    page.off('download', count);
  }
  expect(downloads, 'nothing was saved').toBe(0);
}

/**
 * A GET the page makes to the object store for one named file.
 *
 * The presigned URL's path is an opaque object key, but the download presign
 * forces `Content-Disposition: attachment; filename="…"` through a query
 * parameter — so the name is right there in the URL, which is the only handle a
 * route matcher has on "the request for this particular file".
 */
const storeGetFor =
  (fileName: string) =>
  (url: URL): boolean =>
    url.origin !== APP_ORIGIN && (url.searchParams.get('response-content-disposition') ?? '').includes(fileName);

/**
 * A stored zip, read back out of its central directory.
 *
 * The central directory rather than the local headers, because client-zip
 * streams: it writes each local header before it knows how long the entry will
 * be, leaves the sizes zero and a data-descriptor flag set, and only the
 * central directory at the end carries the real numbers. Nothing here inflates
 * anything — client-zip stores every entry (method 0), which the parse asserts
 * rather than assumes, so an entry's bytes are a slice of the archive.
 */
function readArchive(buf: Buffer): ArchiveEntry[] {
  const EOCD = 0x06054b50;
  const CENTRAL = 0x02014b50;
  const LOCAL = 0x04034b50;

  let eocd = -1;
  for (let i = buf.length - 22; i >= 0 && i >= buf.length - 22 - 0xffff; i--) {
    if (buf.readUInt32LE(i) === EOCD) {
      eocd = i;
      break;
    }
  }
  expect(eocd, 'the file ends in an end-of-central-directory record, so it is a zip').toBeGreaterThanOrEqual(0);

  const count = buf.readUInt16LE(eocd + 10);
  let at = buf.readUInt32LE(eocd + 16);
  // ZIP64 parks these sentinels and moves the real values into records this
  // reader does not parse. The fixtures are a few hundred bytes, so reaching
  // one means something is very wrong and saying so beats a silent misread.
  expect(count, 'not a ZIP64 archive').not.toBe(0xffff);
  expect(at, 'not a ZIP64 archive').not.toBe(0xffffffff);

  const entries: ArchiveEntry[] = [];
  for (let n = 0; n < count; n++) {
    expect(buf.readUInt32LE(at), `central directory entry ${n} is where the record before it said`).toBe(CENTRAL);
    const method = buf.readUInt16LE(at + 10);
    const size = buf.readUInt32LE(at + 24);
    const nameLen = buf.readUInt16LE(at + 28);
    const extraLen = buf.readUInt16LE(at + 30);
    const commentLen = buf.readUInt16LE(at + 32);
    const localAt = buf.readUInt32LE(at + 42);
    const path = buf.toString('utf8', at + 46, at + 46 + nameLen);

    expect(method, `${path} is stored, not compressed`).toBe(0);
    expect(buf.readUInt32LE(localAt), `${path} has a local header where the directory says`).toBe(LOCAL);
    // The data starts after the local header, whose name and extra fields are
    // its own — they are not required to match the central directory's.
    const dataAt = localAt + 30 + buf.readUInt16LE(localAt + 26) + buf.readUInt16LE(localAt + 28);

    entries.push({
      path,
      size,
      bytes: buf.subarray(dataAt, dataAt + size),
      // A zip has no other way to say "folder": a name ending in a separator,
      // with nothing in it.
      isDirectory: path.endsWith('/'),
    });
    at += 46 + nameLen + extraLen + commentLen;
  }
  return entries;
}

const list = () => page.locator('[data-testid="file-list"]');
const rows = () => page.locator('[data-testid="file-row"]');
const rowNamed = (name: string) => rows().filter({ hasText: name });
const band = () => page.getByTestId('command-band');
const dock = () => page.getByRole('complementary', { name: 'Archive' });
const toasts = () => page.locator('[data-sonner-toast]');
