import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type Browser,
  type BrowserContext,
  type Locator,
  type Page,
} from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The account screens and the two mail loops, end to end.
 *
 * What is unprovable anywhere else is the *loop*: a password change that has to
 * reach a second browser, a link that has to travel through a mailbox and back,
 * a session row whose Revoke has to strike a session held somewhere other than
 * this tab. The Go suite proves each endpoint and the vitest suite proves each
 * screen against a mocked client; only a run with two real browser contexts, a
 * real server and a real inbox can say the halves meet.
 *
 * It runs inside `make e2e`, against that run's shared server and freshly
 * seeded database. It never touches the seeded accounts: everything here
 * happens to throwaway addresses this run signs up through the UI and verifies
 * out of Mailpit, so the file it shares a run with still finds
 * `demo@drive.local` with the password the seed gave it.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;
const MAILPIT = process.env.E2E_MAILPIT_API ?? env.DRIVE_MAILPIT_API;

/** Per-run, so a second run against a stack that was not reseeded is a fresh account. */
const RUN = randomUUID().slice(0, 8);

/**
 * Fabricated addresses in the reserved `.example`/`.test` space (RFC 2606).
 * Mailpit accepts anything and delivers it nowhere, which is the point.
 */
const ACCOUNT = {
  email: `throwaway-${RUN}@example.test`,
  name: 'Ada Lovelace',
  initials: 'AL',
};
const RENAMED = { name: 'Grace Hopper', initials: 'GH' };
/** Nobody signs this one up, so `/forgot` has nothing to find behind it. */
const NOBODY = `throwaway-${RUN}-nobody@example.test`;

const FIRST_PASSWORD = `first-pw-${RUN}`;
const CHANGED_PASSWORD = `changed-pw-${RUN}`;
const RESET_PASSWORD = `reset-pw-${RUN}`;

/** What the account's password is right now, as the cases walk it forward. */
let password = FIRST_PASSWORD;

/** The one browser context that stays signed in for the whole file. */
let browserRef: Browser;
let context: BrowserContext;
let page: Page;
/** Mailpit's REST API, on its own request context so no app cookie rides along. */
let mail: APIRequestContext;

test.describe.configure({ mode: 'serial' });

// Argon2 runs for real on both sides of a password change, and two of these
// cases wait on SMTP as well; the default 30 s is not the budget they need.
test.beforeEach(() => test.setTimeout(120_000));

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  browserRef = browser;
  mail = await playwrightRequest.newContext({ baseURL: MAILPIT });

  context = await browser.newContext();
  page = await context.newPage();

  await signUp(page, ACCOUNT.email, FIRST_PASSWORD, ACCOUNT.name);
  await verifyThroughMailbox(page, ACCOUNT.email);
  await signIn(page, ACCOUNT.email, FIRST_PASSWORD);
});

test.afterAll(async () => {
  await context?.close();
  await mail?.dispose();
});

/* ------------------------------------------------------------ account page */

test('the avatar menu opens the account screen, and a rename lands in the avatar', async () => {
  await page.goto(`${BASE}/`);
  await expect(avatar()).toHaveText(ACCOUNT.initials);

  await avatar().click();
  await page.getByRole('menuitem', { name: 'Account settings' }).click();
  await expect(page.getByRole('heading', { name: 'Account', exact: true })).toBeVisible();

  // Stamped on this document. If anything below navigates or reloads, the
  // stamp goes with it -- which is what separates "the avatar updated" from
  // "the page fetched itself again and happened to be right".
  await stamp(page);

  await expect(page.getByLabel('Email')).toHaveValue(ACCOUNT.email);
  await expect(page.getByLabel('Email'), 'the address is shown, not offered for editing').toHaveAttribute(
    'readonly',
    '',
  );

  const name = page.getByLabel('Display name');
  await expect(name).toHaveValue(ACCOUNT.name);
  await name.fill(RENAMED.name);
  await page.getByRole('button', { name: 'Save name' }).click();

  await expect(avatar()).toHaveText(RENAMED.initials);
  expect(await readStamp(page), 'the avatar changed in the document that was already open').toBe(STAMP);

  // The saved name is the new one, so the form has nothing left to send.
  await expect(page.getByRole('button', { name: 'Save name' })).toBeDisabled();

  await avatar().click();
  await expect(page.getByRole('menu')).toContainText(RENAMED.name);
  await page.keyboard.press('Escape');
});

/* ---------------------------------------------------------------- sessions */

test('the sessions list holds both browsers, and Revoke ends the other one', async () => {
  const other = await signedInContext(password);
  try {
    await page.goto(`${BASE}/account`);
    await expect(sessionRows()).toHaveCount(2);

    const current = sessionRows().filter({ hasText: 'This device' });
    await expect(current, 'exactly one row is the browser reading the list').toHaveCount(1);
    await expect(
      current.getByRole('button', { name: 'Revoke' }),
      'the current session offers no Revoke -- signing yourself out of the screen you are on is a trap',
    ).toHaveCount(0);

    const theirs = sessionRows().filter({ hasNotText: 'This device' });
    const revoke = theirs.getByRole('button', { name: 'Revoke' });
    await expect(revoke).toHaveCount(1);
    await revoke.click();
    await expect(sessionRows()).toHaveCount(1);

    // "Signed out on its next request" -- the request, then the screen.
    const me = await other.page.request.get(`${BASE}/api/auth/me`);
    expect(me.status(), 'the revoked cookie no longer identifies anybody').toBe(401);
    await other.page.reload();
    await expect(other.page.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();

    // And this browser, which did the revoking, is untouched.
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Account', exact: true })).toBeVisible();
  } finally {
    await other.context.close();
  }
});

/* -------------------------------------------------------- password change */

test('changing the password signs out the other browser and not this one', async () => {
  const other = await signedInContext(password);
  try {
    await page.goto(`${BASE}/account`);
    await page.getByLabel('Current password').fill(password);
    await page.getByLabel('New password', { exact: true }).fill(CHANGED_PASSWORD);
    await page.getByLabel('Confirm new password').fill(CHANGED_PASSWORD);
    await page.getByRole('button', { name: 'Change password' }).click();

    // Success empties the form; a refusal would have left the fields alone and
    // put the server's message in an alert.
    await expect(page.getByLabel('Current password')).toHaveValue('');
    await expect(page.getByLabel('New password', { exact: true })).toHaveValue('');
    await expect(page.getByRole('alert')).toHaveCount(0);
    password = CHANGED_PASSWORD;

    const theirs = await other.page.request.get(`${BASE}/api/auth/me`);
    expect(theirs.status(), 'the other browser was signed out by the change').toBe(401);
    await other.page.reload();
    await expect(other.page.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();

    const mine = await page.request.get(`${BASE}/api/auth/me`);
    expect(mine.status(), 'the browser that submitted the form kept its session').toBe(200);
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Account', exact: true })).toBeVisible();
  } finally {
    await other.context.close();
  }

  // The new password opens a fresh browser…
  const fresh = await signedInContext(CHANGED_PASSWORD);
  await fresh.context.close();

  // …and the old one does not.
  const stale = await browserRef.newContext();
  try {
    const staleP = await stale.newPage();
    await staleP.goto(`${BASE}/login`);
    await staleP.getByLabel('Email').fill(ACCOUNT.email);
    await staleP.getByLabel('Password').fill(FIRST_PASSWORD);
    await staleP.getByRole('button', { name: 'Sign in' }).click();
    await expect(staleP.getByRole('alert')).toContainText(
      'that email and password combination is not right',
    );
    await expect(staleP.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();
  } finally {
    await stale.close();
  }
});

/* ------------------------------------------------------- forgot → reset */

test('a forgotten password comes back through the mailed link, and only once', async () => {
  await page.goto(`${BASE}/account`);
  await avatar().click();
  await page.getByRole('menuitem', { name: 'Sign out' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();

  await page.getByRole('link', { name: 'Forgot password?' }).click();
  await expect(page.getByRole('heading', { name: 'Reset your password' })).toBeVisible();
  await page.getByLabel('Email').fill(ACCOUNT.email);
  await page.getByRole('button', { name: 'Send reset link' }).click();
  await expect(page.getByRole('heading', { name: 'Check your inbox' })).toBeVisible();

  const message = await waitForMailTo(ACCOUNT.email, 'Reset your Drive password');
  const link = linkFrom(message, '/reset?token=');

  await page.goto(link);
  await expect(page.getByRole('heading', { name: 'Set a new password' })).toBeVisible();
  await setPasswordOnResetScreen(RESET_PASSWORD);
  await expect(page.getByRole('heading', { name: 'Password set' })).toBeVisible();
  password = RESET_PASSWORD;

  // The whole point of the round trip: the new password signs in.
  await page.getByRole('link', { name: 'Go to sign in' }).click();
  await page.getByLabel('Email').fill(ACCOUNT.email);
  await page.getByLabel('Password').fill(RESET_PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();

  // And the link is spent: a second click on the same mail cannot set a third
  // password, whoever is holding the mail by then.
  await page.goto(link);
  await setPasswordOnResetScreen(`again-pw-${RUN}`);
  await expect(page.getByRole('alert')).toContainText('this reset link is invalid or has expired');
  await expect(page.getByRole('link', { name: 'Send another reset link' })).toBeVisible();
});

/* ---------------------------------------------------- resend verification */

test('an unverified account can ask for the verification mail again', async () => {
  const unverified = `throwaway-${RUN}-unverified@example.test`;
  const theirContext = await browserRef.newContext();
  try {
    const theirPage = await theirContext.newPage();
    await signUp(theirPage, unverified, `unverified-pw-${RUN}`, 'Katherine Johnson');
    await waitForMailCount(unverified, 1);

    await theirPage.goto(`${BASE}/login`);
    await theirPage.getByLabel('Email').fill(unverified);
    await theirPage.getByLabel('Password').fill(`unverified-pw-${RUN}`);
    await theirPage.getByRole('button', { name: 'Sign in' }).click();

    await expect(theirPage.getByRole('alert')).toContainText('verify your email first');
    const resend = theirPage.getByRole('button', { name: 'Resend verification' });
    await expect(resend, 'the one refusal a person can act on offers the action').toBeVisible();

    await resend.click();
    await expect(theirPage.getByText('a fresh link is on its way')).toBeVisible();

    // Counted by recipient rather than read: the claim is that a second
    // message was sent, not that it says anything new.
    const inbox = await waitForMailCount(unverified, 2);
    for (const m of inbox) expect(m.Subject).toBe('Verify your Drive account');
  } finally {
    await theirContext.close();
  }
});

/* --------------------------------------------------------------- no oracle */

test('/forgot answers an address with no account exactly as it answers one with', async () => {
  const known = await askForAReset(ACCOUNT.email);
  const unknown = await askForAReset(NOBODY);

  expect(unknown.status, 'both requests are answered the same way').toBe(known.status);
  expect(known.status).toBe(200);

  // The address is the only thing the two screens are allowed to differ by, so
  // it is the only thing normalized away before they are compared.
  expect(unknown.copy.replace(NOBODY, '<address>')).toBe(known.copy.replace(ACCOUNT.email, '<address>'));
  expect(known.copy, 'and the copy is the conditional one').toContain('Check your inbox');
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

const avatar = () => page.getByRole('button', { name: 'Your account' });

/** The rows of the sessions list, found by the section's own heading. */
const sessionRows = () => page.getByRole('region', { name: /signed in/ }).getByRole('listitem');

const STAMP = 'this document has not been reloaded';

async function stamp(target: Page): Promise<void> {
  await target.evaluate((value) => {
    (window as unknown as { __accountSpec?: string }).__accountSpec = value;
  }, STAMP);
}

const readStamp = (target: Page) =>
  target.evaluate(() => (window as unknown as { __accountSpec?: string }).__accountSpec ?? '');

/** Sign up through the form, ending on the conditional confirmation. */
async function signUp(target: Page, email: string, pw: string, name: string): Promise<void> {
  await target.goto(`${BASE}/signup`);
  await target.getByLabel('Name', { exact: true }).fill(name);
  await target.getByLabel('Email').fill(email);
  await target.getByLabel('Password').fill(pw);
  await target.getByRole('button', { name: 'Create account' }).click();
  await expect(target.getByRole('heading', { name: 'Check your inbox' })).toBeVisible();
}

/** Read the verification link out of Mailpit and click it. */
async function verifyThroughMailbox(target: Page, email: string): Promise<void> {
  const message = await waitForMailTo(email, 'Verify your Drive account');
  await target.goto(linkFrom(message, '/verify?token='));
  await expect(target.getByText('Your email is verified. You can sign in now.')).toBeVisible();
}

async function signIn(target: Page, email: string, pw: string): Promise<void> {
  await target.goto(`${BASE}/login`);
  await target.getByLabel('Email').fill(email);
  await target.getByLabel('Password').fill(pw);
  await target.getByRole('button', { name: 'Sign in' }).click();
  await expect(target.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible();
}

/** A second browser holding a session of its own — a different device, in effect. */
async function signedInContext(pw: string): Promise<{ context: BrowserContext; page: Page }> {
  const ctx = await browserRef.newContext();
  const p = await ctx.newPage();
  await signIn(p, ACCOUNT.email, pw);
  return { context: ctx, page: p };
}

async function setPasswordOnResetScreen(pw: string): Promise<void> {
  await page.getByLabel('New password', { exact: true }).fill(pw);
  await page.getByLabel('Confirm new password').fill(pw);
  await page.getByRole('button', { name: 'Set password' }).click();
}

/**
 * Ask `/forgot` for a link and report both halves of the answer: the status the
 * server gave, and the whole screen it produced. Comparing the two is the
 * no-oracle claim -- a server that 404s an unknown address, or a screen that
 * said "no such account", would separate them.
 */
async function askForAReset(email: string): Promise<{ status: number; copy: string }> {
  await page.goto(`${BASE}/forgot`);
  await page.getByLabel('Email').fill(email);
  const [response] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().endsWith('/api/auth/password-reset') && r.request().method() === 'POST',
    ),
    page.getByRole('button', { name: 'Send reset link' }).click(),
  ]);
  await expect(page.getByRole('heading', { name: 'Check your inbox' })).toBeVisible();
  const copy = (await mainRegion().innerText()).replace(/\s+/g, ' ').trim();
  return { status: response.status(), copy };
}

const mainRegion = (): Locator => page.getByRole('main');

/* ------------------------------------------------------------------ mailpit */

/** Mailpit's inbox shapes, cut down to what this file reads. */
type MailSummary = { ID: string; To: { Address: string }[]; Subject: string };
type MailMessage = MailSummary & { Text: string };

const MAIL_DEADLINE = 30_000;
const MAIL_POLL = 200;

/** Every message addressed to `addr`, newest first. */
async function inboxTo(addr: string): Promise<MailSummary[]> {
  // Newest first, and 200 is far more than one run of this file sends, so the
  // window never closes over a message it is waiting for.
  const res = await mail.get('/api/v1/messages?limit=200');
  expect(res.ok(), `Mailpit answered ${res.status()} — is the test stack up?`).toBeTruthy();
  const body = (await res.json()) as { messages: MailSummary[] };
  return body.messages.filter((m) =>
    m.To.some((to) => to.Address.toLowerCase() === addr.toLowerCase()),
  );
}

/** Poll until `addr` has `want` messages, and answer with them. */
async function waitForMailCount(addr: string, want: number): Promise<MailSummary[]> {
  const until = Date.now() + MAIL_DEADLINE;
  let seen: MailSummary[] = [];
  for (;;) {
    seen = await inboxTo(addr);
    if (seen.length >= want) return seen;
    if (Date.now() > until) break;
    await new Promise((resolve) => setTimeout(resolve, MAIL_POLL));
  }
  throw new Error(`mailpit: ${addr} has ${seen.length} message(s), expected ${want}, within 30s`);
}

/**
 * Poll until a message to `addr` with `subject` lands, and fetch its body.
 *
 * Recipient and subject, rather than "newer than the moment I clicked": the
 * addresses are minted per run, so at most one message of each subject ever
 * reaches one of them, and matching on content rather than on a timestamp keeps
 * the wait honest even if the host's clock and the container's disagree.
 */
async function waitForMailTo(addr: string, subject: string): Promise<MailMessage> {
  const until = Date.now() + MAIL_DEADLINE;
  for (;;) {
    const match = (await inboxTo(addr)).find((m) => m.Subject === subject);
    if (match) {
      const res = await mail.get(`/api/v1/message/${encodeURIComponent(match.ID)}`);
      expect(res.ok(), `Mailpit answered ${res.status()} for one message`).toBeTruthy();
      return (await res.json()) as MailMessage;
    }
    if (Date.now() > until) break;
    await new Promise((resolve) => setTimeout(resolve, MAIL_POLL));
  }
  throw new Error(`mailpit: no "${subject}" message to ${addr} within 30s`);
}

/**
 * The app link out of a message body, rebuilt onto the base URL this run is
 * actually driving: the mail carries whatever DRIVE_BASE_URL the server was
 * started with, which is the same host here but need not be.
 */
function linkFrom(message: MailMessage, pathQuery: string): string {
  const escaped = pathQuery.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const found = new RegExp(`${escaped}([A-Za-z0-9_-]+)`).exec(message.Text);
  expect(found, `the mail carries a ${pathQuery}… link`).not.toBeNull();
  return `${BASE}${pathQuery}${found![1]}`;
}
