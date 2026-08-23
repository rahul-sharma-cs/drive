import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type Page,
} from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The motion, in a browser that actually has some.
 *
 * Every timing in this app is a CSS declaration, and jsdom computes no styles
 * at all — so up to now the only evidence that a menu opens in 120ms rather
 * than instantly, or that asking for less movement is honoured, was reading
 * the class strings. That is not evidence: a utility that never compiled, a
 * variant that never matched, a media query written against the wrong feature
 * all leave the source looking exactly right.
 *
 * The three claims here are the ones a browser is needed for:
 *
 *  1. the durations the design actually ships — a menu at 120ms, the command
 *     band's crossfade at 150ms, and nothing moving in that crossfade;
 *  2. that `prefers-reduced-motion: reduce` takes both of them to nothing;
 *  3. that opening a folder runs `document.startViewTransition` exactly once
 *     **and** that the new folder is already on the page when the callback
 *     returns — which is the whole of whether the crossfade animates anything
 *     or snapshots one screen against itself. Nothing about getting that wrong
 *     fails loudly, which is why it is asserted here rather than assumed.
 *
 * It runs inside `make e2e`, against that run's shared server and its freshly
 * seeded database, and signs in as the seeded local account (the two demo
 * users and their password are defined in `server/internal/seed`).
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;

/** The seeded account with an empty root — the other one owns the sample tree. */
const ACCOUNT = { email: 'demo@drive.local', password: 'drive-demo-1' };

const CLIENT = { 'X-Drive-Client': 'web', 'Content-Type': 'application/json' };

/** Per-run names, so a second run against a stack that was not reseeded still fits. */
const RUN = randomUUID().slice(0, 8);
const PARENT = `Motion ${RUN}`;
const CHILD = `Inside ${RUN}`;

let parentId = '';

/** What the design ships, in milliseconds. Both are written in `web/src`. */
const MENU_MS = 120;
const BAND_MS = 150;

test.beforeAll(async () => {
  const api = await playwrightRequest.newContext();
  const login = await api.post(`${BASE}/api/auth/login`, { headers: CLIENT, data: ACCOUNT });
  expect(login.status(), 'the seeded account signs in — did `make e2e` seed this stack?').toBe(200);
  const rootId = (await login.json()).root_id as string;

  parentId = await createFolder(api, rootId, PARENT);
  await createFolder(api, parentId, CHILD);
  await api.dispose();
});

test.describe('the durations the design ships', () => {
  test('opens a menu in 120ms and crossfades the band in 150ms without moving it', async ({ page }) => {
    await signIn(page);
    await openParent(page);

    const band = await bandCrossfade(page);
    expect(Math.round(ms(band.duration))).toBe(BAND_MS);
    // A crossfade and nothing else. The band exists so that selecting a row
    // never shifts the list underneath it, and a transition on anything
    // geometric here would be that shift with a duration attached.
    expect(band.property).toBe('opacity');

    const menu = await openNewMenu(page);
    expect(Math.round(ms(menu.animationDuration))).toBe(MENU_MS);
    expect(Math.round(ms(menu.transitionDuration))).toBe(MENU_MS);
  });

  test('runs one view transition for a folder open, with the new folder already on the page', async ({ page }) => {
    await watchViewTransitions(page);
    await signIn(page);
    await openParent(page);

    // Once through and back first. The trail is a query like any other, and on
    // a cold cache the folder that has just been opened does not know its own
    // name yet — the callback would be reading a breadcrumb that is still
    // being fetched rather than one the router failed to render. Warming it
    // makes the second open a pure rendering question, which is the one being
    // asked. Back is a history event, not a click, so it starts no transition
    // of its own.
    await page.getByRole('link', { name: CHILD, exact: true }).click();
    await expect(currentCrumb(page)).toHaveText(CHILD);
    await page.goBack();
    await expect(currentCrumb(page)).toHaveText(PARENT);
    await expect(page.getByRole('link', { name: CHILD, exact: true })).toBeVisible();

    await resetViewTransitions(page);
    await page.getByRole('link', { name: CHILD, exact: true }).click();
    await expect(currentCrumb(page)).toHaveText(CHILD);

    const seen = await viewTransitions(page);
    // Once. Twice would be a second navigation nobody asked for; none at all
    // would be the helper's own guard deciding this browser has no view
    // transitions, in a project that pins Chromium.
    expect(seen.calls, 'one view transition for one folder open').toBe(1);
    // The frame the browser keeps as "after". If the router had queued the
    // location change as a React transition, this would still read the folder
    // that was on screen when the click happened.
    expect(seen.crumbsInsideCallback, 'the new folder is on the page before the callback returns').toEqual([
      CHILD,
    ]);
  });
});

test.describe('with reduced motion asked for', () => {
  test('holds the menu and the band at a duration nobody can see', async ({ page }) => {
    // `emulateMedia` rather than a context option: this is the media feature
    // the stylesheet actually queries, set on the page under test and nowhere
    // else, so the case above and this one cannot drift apart in how they ask.
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await signIn(page);
    await openParent(page);

    // The stylesheet's reduced-motion block writes 0.01ms rather than 0: a
    // duration of exactly nothing means transitionend never fires, and code
    // that waits for it waits forever.
    const band = ms((await bandCrossfade(page)).duration);
    expect(band, 'the band crossfade is over before it starts').toBeLessThan(1);
    expect(band).toBeGreaterThan(0);

    const menu = await openNewMenu(page);
    expect(ms(menu.animationDuration), 'the menu is simply there').toBeLessThan(1);
    expect(ms(menu.transitionDuration)).toBeLessThan(1);
  });
});

/* ------------------------------------------------------------------ helpers */

interface ViewTransitionLog {
  calls: number;
  crumbsInsideCallback: string[];
}

declare global {
  interface Window {
    __driveViewTransitions?: ViewTransitionLog;
  }
}

/**
 * Counts `document.startViewTransition` calls, and records what the breadcrumb
 * said at the moment each callback finished changing the DOM.
 *
 * The real implementation still runs underneath — the point is to observe the
 * transition the app starts, not to replace it with something that always
 * succeeds. Installed before any page script, so the helper in `web/src/lib`
 * picks up the wrapper rather than the original.
 */
async function watchViewTransitions(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__driveViewTransitions = { calls: 0, crumbsInsideCallback: [] };
    const original = document.startViewTransition?.bind(document);
    if (!original) return;
    document.startViewTransition = ((callback: () => void) => {
      const log = window.__driveViewTransitions;
      if (log) log.calls += 1;
      return original(() => {
        callback();
        const crumb = document.querySelector('nav[aria-label="Breadcrumb"] [aria-current="page"]');
        if (log) log.crumbsInsideCallback.push(crumb?.textContent ?? '');
      });
    }) as typeof document.startViewTransition;
  });
}

/** Signing in is a navigation too; the count that matters starts after it. */
async function resetViewTransitions(page: Page): Promise<void> {
  await page.evaluate(() => {
    window.__driveViewTransitions = { calls: 0, crumbsInsideCallback: [] };
  });
}

async function viewTransitions(page: Page): Promise<ViewTransitionLog> {
  const log = await page.evaluate(() => window.__driveViewTransitions);
  expect(log, 'the init script installed the wrapper').toBeTruthy();
  return log!;
}

/**
 * The band's two layers are stacked, and the one wrapping the selection toolbar
 * is the one that fades in when rows are chosen.
 *
 * Reached with a plain attribute selector and `page.evaluate` rather than a
 * role locator, because with nothing selected that layer is `aria-hidden` and
 * `visibility: hidden` — which is the state being measured, and also the state
 * in which the accessibility tree correctly refuses to hand it over.
 */
async function bandCrossfade(page: Page): Promise<{ duration: string; property: string }> {
  const style = await page.evaluate(() => {
    const toolbar = document.querySelector('[data-testid="command-band"] [role="toolbar"]');
    const layer = toolbar?.parentElement;
    if (!layer) return null;
    const computed = getComputedStyle(layer);
    return { duration: computed.transitionDuration, property: computed.transitionProperty };
  });
  expect(style, 'the command band is on screen with both of its layers').not.toBeNull();
  return style!;
}

/** Opens the rail's New menu and reads the timings off the panel it puts up. */
async function openNewMenu(page: Page): Promise<{ animationDuration: string; transitionDuration: string }> {
  await page.getByRole('button', { name: 'New' }).click();
  const content = page.locator('[data-slot="dropdown-menu-content"]');
  await expect(content).toBeVisible();
  return content.evaluate((el) => {
    const style = getComputedStyle(el);
    return { animationDuration: style.animationDuration, transitionDuration: style.transitionDuration };
  });
}

const currentCrumb = (page: Page) =>
  page.getByRole('navigation', { name: 'Breadcrumb' }).locator('[aria-current="page"]');

/** `0.12s`, `120ms`, or a comma-separated list whose first entry is either. */
function ms(value: string): number {
  const first = value.split(',')[0].trim();
  if (first.endsWith('ms')) return Number(first.slice(0, -2));
  if (first.endsWith('s')) return Number(first.slice(0, -1)) * 1000;
  return Number.NaN;
}

async function signIn(page: Page): Promise<void> {
  await page.goto(`${BASE}/login`);
  await page.getByLabel('Email').fill(ACCOUNT.email);
  await page.getByLabel('Password').fill(ACCOUNT.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();
}

async function openParent(page: Page): Promise<void> {
  await page.goto(`${BASE}/folders/${parentId}`);
  await expect(currentCrumb(page)).toHaveText(PARENT);
  await expect(page.getByRole('link', { name: CHILD, exact: true })).toBeVisible();
}

async function createFolder(api: APIRequestContext, parent: string, name: string): Promise<string> {
  const res = await api.post(`${BASE}/api/folders`, { headers: CLIENT, data: { parent_id: parent, name } });
  expect(res.status(), `created ${name}`).toBe(201);
  return (await res.json()).id as string;
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
