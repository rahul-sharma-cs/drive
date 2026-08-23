import { test, expect, type Locator, type Page } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The file browser, in a real browser.
 *
 * Everything here is a claim jsdom cannot answer, because jsdom computes no
 * layout: whether the list moves when a row is selected, whether a command is
 * reachable at 390px rather than merely present in the DOM, whether a native
 * drag lights the upload drop zone. The unit suite covers the behaviour these
 * screens have; this one covers the geometry, which is where the complaints
 * that started this work actually lived.
 *
 * It runs inside `make e2e`, against that run's shared server and its freshly
 * seeded database, and signs in as the seeded local account (the two demo
 * users and their password are defined in `server/internal/seed`). Every
 * fixture it makes is named with a per-run id, so a second run against a stack
 * that was not reseeded does not collide with the first.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;

/**
 * The seeded local account. `demo@drive.local` is the one with an empty root —
 * `rahul@drive.local` owns the sample tree the Go and search tests read, and
 * this spec creates and deletes things.
 */
const ACCOUNT = { email: 'demo@drive.local', password: 'drive-demo-1' };

const RUN = randomUUID().slice(0, 8);
const FIXTURE = `Spec ${RUN}`;
/** Folders sort ahead of files, and among themselves by name. */
const FOLDERS = ['Alpha', 'Beta'] as const;
const UPLOADED = 'notes.txt';
/** Sorts after both fixture folders, so the row order the cases rely on holds. */
const CREATED_IN_UI = `Zulu ${RUN}`;

const CLIENT = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

/** One signed-in page for the whole file: signing in once per case proves nothing extra. */
let page: Page;
let fixtureId: string;
/** The last of the fixture's subfolders, which nothing is ever put into. */
let emptyFolderId: string;

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  page = await browser.newPage();

  // The root id comes from the API; the cookie it sets is dropped again so the
  // login screen is genuinely exercised below.
  const login = await page.request.post(`${BASE}/api/auth/login`, { headers: CLIENT, data: ACCOUNT });
  expect(login.status(), 'the seeded account signs in — did `make e2e` seed this stack?').toBe(200);
  const rootId = (await login.json()).root_id as string;

  fixtureId = await createFolder(rootId, FIXTURE);
  for (const name of FOLDERS) emptyFolderId = await createFolder(fixtureId, name);
  await page.context().clearCookies();

  await signIn(page);
  await openFixture();

  // The third item is a real upload through the hidden input the New menu
  // drives, so the row the layout cases measure got there the way a person's
  // would. `setInputFiles` reaches the input directly, without the menu, which
  // is the case `openPicker`'s fallback exists for.
  const dir = mkdtempSync(join(tmpdir(), 'drive-browser-'));
  const fixtureFile = join(dir, UPLOADED);
  writeFileSync(fixtureFile, 'the quick brown fox\n'.repeat(64));
  await page.getByLabel('Upload files').setInputFiles(fixtureFile);
  await expect(rowNamed(UPLOADED)).toBeVisible({ timeout: 60_000 });

  // The dock parks in the bottom-right corner once it has something to report,
  // and below `sm` it spans the width. Clearing it keeps it out of the way of
  // the narrow-viewport case.
  await page.getByRole('button', { name: 'Clear finished' }).click();
  await expect(page.getByRole('complementary', { name: 'Uploads' })).toBeHidden();

  await expect(rows()).toHaveCount(3);
});

test.afterAll(async () => {
  await page?.close();
});

/* ------------------------------------------------------ the list never moves */

test('selecting a row does not move the list', async () => {
  await openFixture();

  const first = rows().first();
  const before = await boxOf(first);
  const bandIdle = await boxOf(band());
  await expect(trashCommand()).toBeHidden();

  await first.getByRole('checkbox').click();
  await expect(first).toHaveAttribute('data-selected', '');
  await expect(trashCommand()).toBeVisible();
  await expect(band().getByText('1 selected')).toBeVisible();

  const selected = await boxOf(first);
  expect(selected.y, 'the first row is where it was before the selection').toBe(before.y);
  expect((await boxOf(band())).height, 'the band did not change height either').toBe(bandIdle.height);
  // And what it is the same height as: 4px above one 48px row, 12px below.
  expect(bandIdle.height).toBe(64);

  // Out of the selection by the band's own control…
  await band().getByRole('button', { name: 'Clear the selection' }).click();
  await expect(trashCommand()).toBeHidden();
  expect((await boxOf(first)).y).toBe(before.y);

  // …and out of it by Escape, which the list handles rather than the band.
  await first.getByRole('checkbox').click();
  await expect(trashCommand()).toBeVisible();
  expect((await boxOf(first)).y).toBe(before.y);
  await first.getByRole('checkbox').press('Escape');
  await expect(trashCommand()).toBeHidden();
  expect((await boxOf(first)).y).toBe(before.y);
});

/* ------------------------------------------------------------- 390px commands */

test('every selected command is reachable at 390px', async () => {
  await page.setViewportSize({ width: 390, height: 780 });
  try {
    await openFixture();

    const first = rows().first();
    const before = await boxOf(first);
    const idleHeight = (await boxOf(band())).height;

    // A file, not a folder: that is the widest the toolbar ever gets, because
    // Download and Rename are both single-item commands.
    await rowNamed(UPLOADED).getByRole('checkbox').click();
    await expect(trashCommand()).toBeVisible();

    // Wrapping onto a second line is the failure this width is here to catch:
    // it is what used to push the list down the moment anything was selected.
    expect((await boxOf(band())).height).toBe(idleHeight);
    expect((await boxOf(first)).y).toBe(before.y);

    const bar = toolbar();
    const barBox = await boxOf(bar);
    expect(barBox.x).toBeGreaterThanOrEqual(0);
    expect(barBox.x + barBox.width).toBeLessThanOrEqual(390);

    // Not clipped — a `hidden` here would put commands permanently out of
    // reach at exactly the width where the icons are all a person has.
    expect(await bar.evaluate((el) => getComputedStyle(el).overflowX)).toBe('auto');

    const commands: Locator[] = [
      bar.getByRole('button', { name: 'Clear the selection' }),
      bar.getByRole('link', { name: 'Download' }),
      bar.getByRole('button', { name: 'Rename' }),
      bar.getByRole('button', { name: 'Move to' }),
      bar.getByRole('button', { name: 'Copy to' }),
      bar.getByRole('button', { name: 'Trash' }),
    ];

    // Reachable means: scroll the row to it, and it is then wholly inside the
    // scroller and inside the window. Present-in-the-DOM is what jsdom can
    // already say, and it is not the same claim.
    for (const command of commands) {
      await command.scrollIntoViewIfNeeded();
      await expect(command).toBeVisible();
      await expectWithin(command, bar);
    }

    // And at the far end of the scroll, where a clipped last command would be.
    await bar.evaluate((el) => {
      el.scrollLeft = el.scrollWidth;
    });
    for (const command of commands.slice(-2)) await expectWithin(command, bar);

    expect((await boxOf(first)).y, 'scrolling the commands did not move the rows').toBe(before.y);
  } finally {
    await page.setViewportSize({ width: 1280, height: 800 });
  }
});

/* --------------------------------------------------------------- right-click */

test('right-clicking a row opens its menu, and Rename opens the dialog for it', async () => {
  await openFixture();

  const target = rowNamed(FOLDERS[1]);
  await target.click({ button: 'right' });

  const menu = page.locator('[data-slot="context-menu-content"]');
  await expect(menu).toBeVisible();
  await menu.getByRole('menuitem', { name: 'Rename' }).click();

  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('heading', { name: 'Rename' })).toBeVisible();
  // The dialog answers for the row the menu was opened from, not for whichever
  // row happens to be selected or focused.
  await expect(dialog.getByLabel('Name')).toHaveValue(FOLDERS[1]);

  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toBeHidden();
  await expect(rowNamed(FOLDERS[1])).toBeVisible();
});

test('a right-click too quick to be a menu click runs no command', async () => {
  await openFixture();

  // The bottom row, with the window cut off just under it: with no room below
  // the cursor the menu opens upward, over the point it was summoned from. A
  // menu item lands under the cursor while the menu is still growing into
  // place, and the button coming back up there is a `pointerup` on a command —
  // "Move to trash", at the bottom of the menu, is the one at this edge.
  const viewport = page.viewportSize()!;
  const target = rows().last();
  const before = await rows().count();
  const full = await boxOf(target);
  await page.setViewportSize({ width: viewport.width, height: Math.round(full.y + full.height + 16) });

  try {
    const box = await boxOf(target);
    const x = Math.round(box.x + 140);
    const y = Math.round(box.y + box.height / 2);

    // Pressed and released without pausing, the way a fast hand does it.
    await page.mouse.move(x, y);
    await page.mouse.down({ button: 'right' });
    await page.mouse.up({ button: 'right' });
    await expect(contextMenu()).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(rowNamed(UPLOADED)).toBeVisible();
    expect(await rows().count()).toBe(before);
    await page.keyboard.press('Escape');
    await expect(contextMenu()).toHaveCount(0);

    // The same gesture with the menu's opening stretched out, so the release
    // lands in the middle of it every run rather than in whichever frame this
    // machine happened to reach. The animation is the whole of the window in
    // which an item is under the cursor, so this is the deterministic form of
    // the case above.
    const slowly = await page.addStyleTag({
      content: '[data-slot="context-menu-content"] { animation-duration: 2s !important; }',
    });
    try {
      await page.mouse.move(x, y);
      await page.mouse.down({ button: 'right' });
      await contextMenu().waitFor();
      await page.mouse.up({ button: 'right' });
      await expect(contextMenu()).toBeVisible();
      await expect(page.getByRole('dialog')).toHaveCount(0);
      await expect(rowNamed(UPLOADED)).toBeVisible();
      expect(await rows().count()).toBe(before);
      await page.keyboard.press('Escape');
      await expect(contextMenu()).toHaveCount(0);
    } finally {
      // addStyleTag hands back an ElementHandle<Node>, and remove() is on
      // Element. It is the <style> this call just injected.
      await slowly.evaluate((tag) => (tag as HTMLStyleElement).remove());
    }

    // The control: a deliberate click on an item still runs it, at this same
    // edge and within the window the guard covers.
    await page.mouse.click(x, y, { button: 'right' });
    await contextMenu().getByRole('menuitem', { name: 'Rename' }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByLabel('Name')).toHaveValue(UPLOADED);
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(dialog).toBeHidden();
  } finally {
    await page.setViewportSize(viewport);
  }
});

test('the row menu opens on screen at 390px', async () => {
  const viewport = page.viewportSize()!;
  await page.setViewportSize({ width: 390, height: 780 });

  try {
    await openFixture();
    const box = await boxOf(rows().first());
    const y = Math.round(box.y + box.height / 2);

    // 208px of menu on 390px of screen: near the middle there is not that much
    // room on either side of the cursor, and the menu can only go left or
    // right of it. x=195 is the worst case; the other two are the sides it can
    // still take whole.
    for (const x of [150, 195, 250]) {
      await page.mouse.click(x, y, { button: 'right' });
      await expect(contextMenu()).toBeVisible();

      // Measured after it has finished growing: `toBeVisible` is true from the
      // first frame of the animation, and a box read there is the 95% one.
      await settled(contextMenu());
      const menu = await boxOf(contextMenu());
      expect(menu.width, `the menu opened at x=${x} has width`).toBeGreaterThan(0);
      expect(menu.x, `the menu opened at x=${x} starts on screen`).toBeGreaterThanOrEqual(0);
      expect(menu.x + menu.width, `the menu opened at x=${x} ends on screen`).toBeLessThanOrEqual(390);

      await page.keyboard.press('Escape');
      await expect(contextMenu()).toHaveCount(0);
    }
  } finally {
    await page.setViewportSize(viewport);
  }
});

/* --------------------------------------------------------------- the New menu */

test('New folder from the rail creates in the folder on screen', async () => {
  await openFixture();
  await expect(rowNamed(CREATED_IN_UI)).toHaveCount(0);

  await page.getByRole('button', { name: 'New' }).click();
  await page.getByRole('menuitem', { name: 'New folder' }).click();

  const dialog = page.getByRole('dialog');
  await expect(dialog.getByRole('heading', { name: 'New folder' })).toBeVisible();
  await dialog.getByLabel('Name').fill(CREATED_IN_UI);
  await dialog.getByRole('button', { name: 'Create' }).click();

  // The list on screen IS the read of this folder's children, so a row here is
  // the claim: it landed in the folder that was open, not in the root.
  await expect(rowNamed(CREATED_IN_UI)).toBeVisible();
  await expect(rows()).toHaveCount(4);
});

/* ------------------------------------------------------------------ keyboard */

test('arrow keys walk the list and Enter opens the focused folder', async () => {
  await openFixture();

  const startedAt = page.url();

  await list().focus();
  await page.keyboard.press('ArrowDown');
  await expect(rows().first()).toHaveAttribute('data-focused', '');
  // Already on the first row: ArrowUp must stay there rather than wrap to the last.
  await page.keyboard.press('ArrowUp');
  await expect(rows().first()).toHaveAttribute('data-focused', '');
  await page.keyboard.press('ArrowDown');

  const second = rows().nth(1);
  await expect(second).toHaveAttribute('data-focused', '');
  const name = (await second.getByRole('link').textContent())?.trim() ?? '';
  expect(name, 'the second row is a folder, so Enter has something to open').not.toBe('');

  await page.keyboard.press('Enter');

  await expect(page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]')).toHaveText(
    name,
  );
  expect(page.url()).toContain('/folders/');
  expect(page.url(), 'Enter went somewhere new').not.toBe(startedAt);
});

/* ----------------------------------------------------------------- drag gate */

test('dragging a row does not offer to upload it', async () => {
  await openFixture();
  await recordDrags();

  const source = rowNamed(UPLOADED);
  const folderRow = rowNamed(FOLDERS[0]);

  await source.hover();
  await page.mouse.down();

  // Two moves over one element, because that is how the events arrive: the
  // first is its `dragenter`, the second the `dragover` the app listens on.
  await folderRow.hover({ position: { x: 200, y: 24 } });
  await folderRow.hover({ position: { x: 220, y: 24 } });

  // The drag is real and the app is reading it — the folder under the pointer
  // lights up as a move target. Without this, the assertion beside it would
  // pass just as well on a drag that never started.
  await expect(folderRow).toHaveClass(DROP_TARGET);
  await expect(dropHint()).toBeHidden();

  // Now over the list but over nothing that takes a node. This is the dragover
  // the upload drop zone itself answers, and a file that is already uploaded is
  // not something to offer to upload.
  await recordDrags();
  const header = columnHeader();
  await header.hover({ position: { x: 200, y: 18 } });
  await header.hover({ position: { x: 220, y: 18 } });
  await expect(folderRow).not.toHaveClass(DROP_TARGET);
  expect(await draggedTypes(), 'the drop zone saw a dragover carrying a node').toContain(NODE_DRAG_TYPE);
  await expect(dropHint()).toBeHidden();

  // Released over nothing that cancelled the dragover, so there is no drop and
  // nothing has been moved.
  await page.mouse.up();
  await expect(rows()).toHaveCount(4);
  await expect(rowNamed(UPLOADED)).toBeVisible();

  // The control. Without it, "the hint stayed hidden" is also what a drop zone
  // that never lights up for anything would say.
  await simulateFileDrag('dragover');
  await expect(dropHint()).toBeVisible();
  await simulateFileDrag('dragleave');
  await expect(dropHint()).toBeHidden();
});

test('a folder says it takes drops once it has rows in it', async () => {
  // The empty state carries the invitation while there is nothing there; after
  // the first file it is gone, and the hint under the list is what is left.
  // It is gated in CSS on the list having rows, so only a browser can say.
  await openFixture();
  await expect(dropHintIdle()).toBeVisible();

  await page.goto(`${BASE}/folders/${emptyFolderId}`);
  await expect(page.getByText('This folder is empty.')).toBeVisible();
  await expect(dropHintIdle()).toBeHidden();
});

/* ------------------------------------------------------------------ helpers */

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

async function signIn(target: Page): Promise<void> {
  await target.goto(`${BASE}/login`);
  await target.getByLabel('Email').fill(ACCOUNT.email);
  await target.getByLabel('Password').fill(ACCOUNT.password);
  await target.getByRole('button', { name: 'Sign in' }).click();
  await expect(target.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();
}

/** Back to the fixture folder with nothing selected, whatever the last case left behind. */
async function openFixture(): Promise<void> {
  await page.goto(`${BASE}/folders/${fixtureId}`);
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]')).toHaveText(
    FIXTURE,
  );
  await expect(list()).toBeVisible();
  await expect(trashCommand()).toBeHidden();
}

const list = () => page.locator('[data-testid="file-list"]');
const rows = () => page.locator('[data-testid="file-row"]');
const rowNamed = (name: string) => rows().filter({ hasText: name });
const band = () => page.getByTestId('command-band');
const contextMenu = () => page.locator('[data-slot="context-menu-content"]');
const toolbar = () => page.getByRole('toolbar', { name: 'Selection actions' });
const trashCommand = () => band().getByRole('button', { name: 'Trash' });
const dropHint = () => page.getByText('Drop to upload here');
const dropHintIdle = () => page.getByText(/Drag files or folders in from your computer/);

/**
 * The bare `ring-teal` a row wears while a drag is over it. Anchored on both
 * sides because the row also carries `data-[focused]:ring-teal/40` at all
 * times, and a loose /ring-teal/ would match that on every row forever.
 */
const DROP_TARGET = /(^|\s)ring-teal(\s|$)/;

async function boxOf(locator: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await locator.boundingBox();
  expect(box, 'the element has a box to measure').not.toBeNull();
  return box!;
}

/** Waits out an element's entry animation, so what is measured is where it lands. */
async function settled(locator: Locator): Promise<void> {
  await locator.evaluate(async (el) => {
    await Promise.all(el.getAnimations().map((animation) => animation.finished));
  });
}

/** `child` is wholly inside `parent`, give or take the sub-pixel a layout rounds. */
async function expectWithin(child: Locator, parent: Locator): Promise<void> {
  const c = await boxOf(child);
  const p = await boxOf(parent);
  const name = (await child.getAttribute('aria-label')) ?? '';
  expect(c.width, `${name} has width`).toBeGreaterThan(0);
  expect(c.x, `${name} starts inside the toolbar`).toBeGreaterThanOrEqual(p.x - 1);
  expect(c.x + c.width, `${name} ends inside the toolbar`).toBeLessThanOrEqual(p.x + p.width + 1);
  expect(c.y, `${name} sits inside the toolbar`).toBeGreaterThanOrEqual(p.y - 1);
  expect(c.y + c.height).toBeLessThanOrEqual(p.y + p.height + 1);
}

/** The MIME type an internal row drag carries, from `features/browser/dnd.ts`. */
const NODE_DRAG_TYPE = 'application/x-drive-node';

/**
 * Start recording the `dragover` events reaching the document, discarding
 * anything recorded before. Playwright delivers a `dragenter` when the drag
 * crosses into an element and a `dragover` on each move inside it, so what the
 * app's own handlers see is worth reading back rather than assuming.
 */
async function recordDrags(): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __dragTypes: string[]; __dragHooked?: true };
    w.__dragTypes = [];
    if (w.__dragHooked) return;
    w.__dragHooked = true;
    document.addEventListener(
      'dragover',
      (event) => w.__dragTypes.push(...Array.from((event as DragEvent).dataTransfer?.types ?? [])),
      true,
    );
  });
}

const draggedTypes = () =>
  page.evaluate(() => (window as unknown as { __dragTypes: string[] }).__dragTypes);

/** The row of column labels directly above the list, which takes no drop of its own. */
const columnHeader = () => list().locator('xpath=preceding-sibling::div[1]');

/**
 * A drag carrying a file, dispatched at the drop zone. Synthesized rather than
 * driven with the mouse because there is no OS-level file drag to start from,
 * and what it has to carry is exactly what the zone gates on: `Files` in
 * `dataTransfer.types`.
 */
async function simulateFileDrag(type: 'dragover' | 'dragleave'): Promise<void> {
  await page.evaluate((eventType) => {
    const zone = document.querySelector('[data-testid="drop-zone"]');
    if (!zone) throw new Error('no drop zone on this screen');
    const dataTransfer = new DataTransfer();
    dataTransfer.items.add(new File(['x'], 'dragged.txt', { type: 'text/plain' }));
    zone.dispatchEvent(new DragEvent(eventType, { bubbles: true, cancelable: true, dataTransfer }));
  }, type);
}
