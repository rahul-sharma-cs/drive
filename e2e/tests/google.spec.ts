import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type Browser,
  type BrowserContext,
  type Locator,
  type Page,
  type Request,
} from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { mailbox, readEnvFile } from './support/mailpit';

/**
 * Sign in with Google, through a browser.
 *
 * What is unprovable anywhere else is the *round trip*: the browser leaves this
 * origin for the provider and comes back to a different one, carrying a cookie
 * scoped to two routes, and has to arrive signed in. The Go suite proves each
 * endpoint against an in-process stub and the vitest suite proves each screen
 * against a mocked client; neither of them ever leaves the origin, and the
 * failure mode this file exists for -- a flow that verifies perfectly and lands
 * the person back on /login anyway -- looks identical to success in both.
 *
 * The provider is `server/cmd/oidcstub`, which `make e2e` starts on the loopback
 * address `.env.test` names as DRIVE_GOOGLE_ISSUER. It signs a token about
 * whoever `POST /control` last said, and its /authorize has no consent screen,
 * so a click here is one 302 to it and one back.
 *
 * It never touches the seeded accounts: the addresses are per-run in the
 * reserved `.example`/`.test` space (RFC 2606) and the subjects are fabricated
 * per-run strings, so nothing here collides with the other files in the run or
 * with a second run against a stack that was not reseeded.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const env = readEnvFile(join(REPO, '.env.test'));
const BASE = process.env.E2E_BASE_URL ?? `http://localhost${env.DRIVE_ADDR}`;
const MAILPIT = process.env.E2E_MAILPIT_API ?? env.DRIVE_MAILPIT_API;
/**
 * The fake provider, at exactly the string the server was configured to
 * discover — read from the same file the server and `make e2e` read, with no
 * override, because a second source for it is a second thing that can disagree
 * with the issuer the stub publishes.
 */
const ISSUER = env.DRIVE_GOOGLE_ISSUER;

const RUN = randomUUID().slice(0, 8);

/** The person this file signs in as. The subject is what the provider calls them. */
const GOOGLE_USER = {
  subject: `stub-subject-${RUN}-a`,
  email: `google-${RUN}@example.test`,
  name: 'Ada Lovelace',
};

/**
 * The pre-hijack: an address somebody signed up with a password and never
 * verified, which the provider then vouches for as somebody else's.
 */
const SQUATTED = {
  subject: `stub-subject-${RUN}-b`,
  email: `squatted-${RUN}@example.test`,
  password: `squatter-pw-${RUN}`,
};

const RESET_PASSWORD = `reset-pw-${RUN}`;
/** Made while signed in with Google, and looked for again after the second sign-in. */
const FOLDER = `google-run-${RUN}`;

/** The one browser context the cases walk forward together. */
let browserRef: Browser;
let context: BrowserContext;
let page: Page;
/** Mailpit's REST API, on its own request context so no app cookie rides along. */
let mail: APIRequestContext;
/** The stub's control channel, likewise: it is a different origin entirely. */
let stub: APIRequestContext;

const { waitForMailTo, linkFrom } = mailbox(() => mail, BASE);

test.describe.configure({ mode: 'serial' });

// A sign-in here is two navigations and an RSA verification, and one case waits
// on SMTP and runs Argon2 twice; the default 30 s is not the budget they need.
test.beforeEach(() => test.setTimeout(120_000));

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  browserRef = browser;
  mail = await playwrightRequest.newContext({ baseURL: MAILPIT });
  stub = await playwrightRequest.newContext({ baseURL: ISSUER });

  context = await browser.newContext();
  page = await context.newPage();
});

test.afterAll(async () => {
  await context?.close();
  await mail?.dispose();
  await stub?.dispose();
});

/* --------------------------------------------------------------- signing in */

test('Continue with Google leaves the origin and comes back signed in', async () => {
  await provider(GOOGLE_USER);

  await page.goto(`${BASE}/login`);
  const link = page.getByRole('link', { name: 'Continue with Google' });
  // The href, not just the click: a <Link> or a fetch would both look like a
  // working button here and neither can carry a top-level navigation off-origin.
  await expect(link).toHaveAttribute('href', '/api/auth/google/start');

  await link.click();
  await expect(breadcrumb()).toBeVisible();

  await page.goto(`${BASE}/account`);
  await expect(identityRows()).toHaveCount(1);
  await expect(identityRows()).toContainText('Google');
  await expect(identityRows(), 'the row names the address the link was made with').toContainText(
    GOOGLE_USER.email,
  );
  await expect(
    unlink(),
    'the only way into this account cannot be taken away from it',
  ).toBeDisabled();
  await expect(passwordSection()).toContainText('You sign in with Google');
});

test('a second sign-in on the same subject is the same account, with the same files', async () => {
  await page.goto(`${BASE}/`);
  await newFolder(FOLDER);

  await signOut();

  // From /signup this time, and with the stub told nothing new: the subject is
  // what the account is found by, so the second click is the same person.
  await page.goto(`${BASE}/signup`);
  await page.getByRole('link', { name: 'Continue with Google' }).click();
  await expect(breadcrumb()).toBeVisible();

  await expect(rowNamed(FOLDER), 'the folder made before signing out is still there').toBeVisible();
});

/* ----------------------------------------------------------------- unlinking */

test('unlinking is refused while Google is the only way in', async () => {
  // Through the API rather than the screen: the button is disabled, so the only
  // way to ask is the way somebody who bypassed the screen would ask.
  const listed = await page.request.get(`${BASE}/api/auth/identities`);
  expect(listed.status()).toBe(200);
  const { items } = (await listed.json()) as { items: { id: string }[] };
  expect(items, 'exactly one identity is linked').toHaveLength(1);

  const refused = await page.request.delete(`${BASE}/api/auth/identities/${items[0].id}`, {
    headers: { 'X-Drive-Client': 'web' },
  });
  expect(refused.status()).toBe(409);
  expect((await refused.json()).code).toBe('unsupported');

  await page.goto(`${BASE}/account`);
  await expect(identityRows(), 'and the row is still there after a reload').toHaveCount(1);
});

/* ------------------------------------------------------------ a failed trip */

test('a callback that proves nothing lands on /login?error=google, quietly', async () => {
  // A fresh context: no drive_oauth cookie, which is what a replayed or forged
  // callback arrives without.
  const stranger = await browserRef.newContext();
  try {
    const p = await stranger.newPage();

    await p.goto(`${BASE}/api/auth/google/callback?code=x&state=y`);
    expect(p.url()).toBe(`${BASE}/login?error=google`);
    const alert = p.getByRole('alert');
    await expect(alert).toContainText("Google sign-in didn't complete");
    const me = await p.request.get(`${BASE}/api/auth/me`);
    expect(me.status(), 'nothing was signed in on the way past').toBe(401);

    await alert.getByRole('button', { name: 'Dismiss' }).click();
    await expect(p.getByRole('alert')).toHaveCount(0);
    const dismissed = new URL(p.url());
    expect(dismissed.pathname).toBe('/login');
    expect(
      dismissed.searchParams.has('error'),
      'dismissing takes the parameter out of the URL, so a reload is not shown it again',
    ).toBe(false);

    // Cancel is not a failure, and is not reported as one.
    await p.goto(`${BASE}/api/auth/google/callback?error=access_denied&state=y`);
    expect(p.url()).toBe(`${BASE}/login`);
    await expect(p.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();
    await expect(p.getByRole('alert'), 'pressing Cancel is told nothing went wrong').toHaveCount(0);
  } finally {
    await stranger.close();
  }
});

/* ------------------------------------------- a password, and then unlinking */

test('a password through Forgot password makes Unlink work, and outlives it', async () => {
  await signOut();

  await page.getByRole('link', { name: 'Forgot password?' }).click();
  await page.getByLabel('Email').fill(GOOGLE_USER.email);
  await page.getByRole('button', { name: 'Send reset link' }).click();
  await expect(page.getByRole('heading', { name: 'Check your inbox' })).toBeVisible();

  const message = await waitForMailTo(GOOGLE_USER.email, 'Reset your Drive password');
  await page.goto(linkFrom(message, '/reset?token='));
  await expect(page.getByRole('heading', { name: 'Set a new password' })).toBeVisible();
  await page.getByLabel('New password', { exact: true }).fill(RESET_PASSWORD);
  await page.getByLabel('Confirm new password').fill(RESET_PASSWORD);
  await page.getByRole('button', { name: 'Set password' }).click();
  await expect(page.getByRole('heading', { name: 'Password set' })).toBeVisible();

  // An account that has never had a password now has one -- which is the whole
  // reason the account screen sends people here rather than offering a form.
  await signIn(page, GOOGLE_USER.email, RESET_PASSWORD);

  await page.goto(`${BASE}/account`);
  await expect(unlink()).toBeEnabled();

  // Unlink asks first, and the question is the whole point: there is no undo,
  // and getting the link back means signing out and going round Google again.
  let deletes = 0;
  const countDeletes = (req: Request) => {
    if (req.method() === 'DELETE' && req.url().includes('/api/auth/identities/')) deletes += 1;
  };
  page.on('request', countDeletes);
  try {
    await unlink().click();
    const asking = page.getByRole('dialog', { name: 'Unlink Google?' });
    await expect(asking).toBeVisible();

    // Cancel is a no-op all the way down, not just on screen: a dialog that
    // fired the DELETE and then drew the row back would look identical here
    // without the request count.
    await asking.getByRole('button', { name: 'Cancel' }).click();
    await expect(asking).toBeHidden();
    expect(deletes, 'Cancel asked the server for nothing').toBe(0);
    await expect(identityRows(), 'and the row it was asked about is still there').toHaveCount(1);

    // And then the same question, answered.
    await unlink().click();
    await expect(asking).toBeVisible();
    await asking.getByRole('button', { name: 'Unlink' }).click();
    await expect(page.getByText('Sign-in method removed')).toBeVisible();
    await expect(asking).toBeHidden();
    await expect(identityRows()).toHaveCount(0);
    expect(deletes, 'exactly one DELETE, from the dialog').toBe(1);
  } finally {
    page.off('request', countDeletes);
  }

  // The password is the way in now, and it works on its own.
  await signOut();
  await signIn(page, GOOGLE_USER.email, RESET_PASSWORD);

  // And the same subject links again -- by the address, which the provider has
  // just vouched for and this account verified when the link was first made.
  await provider(GOOGLE_USER);
  await signOut();
  await page.getByRole('link', { name: 'Continue with Google' }).click();
  await expect(breadcrumb()).toBeVisible();

  await page.goto(`${BASE}/account`);
  await expect(identityRows()).toHaveCount(1);
  await expect(unlink(), 'the account has a password, so the link is optional now').toBeEnabled();
});

/* ------------------------------------------------------------- the pre-hijack */

test('a link onto an unverified account leaves its password behind', async () => {
  const theirs = await browserRef.newContext();
  try {
    const p = await theirs.newPage();

    // Somebody signs the address up and never proves they own it. On an
    // open-signup deployment that is anybody, about anybody's address.
    await signUp(p, SQUATTED.email, SQUATTED.password, 'Katherine Johnson');

    // The provider then says the address belongs to somebody else entirely.
    await provider(SQUATTED);
    await p.goto(`${BASE}/login`);
    await p.getByRole('link', { name: 'Continue with Google' }).click();
    await expect(breadcrumb(p)).toBeVisible();

    await p.goto(`${BASE}/account`);
    await expect(identityRows(p)).toHaveCount(1);
    await expect(
      unlink(p),
      'the link activated the account and took the unproven password with it',
    ).toBeDisabled();

    await signOut(p);
    await p.getByLabel('Email').fill(SQUATTED.email);
    await p.getByLabel('Password').fill(SQUATTED.password);
    await p.getByRole('button', { name: 'Sign in' }).click();
    await expect(p.getByRole('alert')).toContainText(
      'that email and password combination is not right',
    );
    await expect(breadcrumb(p), 'the password nobody proved opens nothing').toHaveCount(0);
    const me = await p.request.get(`${BASE}/api/auth/me`);
    expect(me.status()).toBe(401);
  } finally {
    await theirs.close();
  }
});

/* ------------------------------------------------------------------ helpers */

/**
 * Tell the stub who its next authorization is about.
 *
 * It is one identity at a time and it is global to the provider, which is why
 * this file is serial and why nothing else in the run touches the stub.
 */
async function provider(who: { subject: string; email: string; name?: string }): Promise<void> {
  const res = await stub.post('/control', {
    data: {
      sub: who.subject,
      email: who.email,
      email_verified: true,
      name: who.name ?? '',
    },
  });
  expect(res.ok(), `the stub took the identity (it answered ${res.status()})`).toBeTruthy();
}

const breadcrumb = (target: Page = page): Locator =>
  target.getByRole('navigation', { name: 'Breadcrumb' });

const rows = (target: Page = page): Locator => target.locator('[data-testid="file-row"]');
const rowNamed = (name: string, target: Page = page): Locator =>
  rows(target).filter({ hasText: name });

/** The account screen's linked-account rows, found by the section's own heading. */
const identityRows = (target: Page = page): Locator =>
  target.getByRole('region', { name: 'Sign-in methods' }).getByRole('listitem');

const unlink = (target: Page = page): Locator =>
  identityRows(target).getByRole('button', { name: 'Unlink' });

const passwordSection = (target: Page = page): Locator =>
  target.getByRole('region', { name: 'Password' });

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

/** Sign up through the form, ending on the conditional confirmation. Nobody verifies. */
async function signUp(target: Page, email: string, pw: string, name: string): Promise<void> {
  await target.goto(`${BASE}/signup`);
  await target.getByLabel('Name', { exact: true }).fill(name);
  await target.getByLabel('Email').fill(email);
  await target.getByLabel('Password').fill(pw);
  await target.getByRole('button', { name: 'Create account' }).click();
  await expect(target.getByRole('heading', { name: 'Check your inbox' })).toBeVisible();
}

async function signIn(target: Page, email: string, pw: string): Promise<void> {
  await target.goto(`${BASE}/login`);
  await target.getByLabel('Email').fill(email);
  await target.getByLabel('Password').fill(pw);
  await target.getByRole('button', { name: 'Sign in' }).click();
  await expect(breadcrumb(target)).toBeVisible();
}

/** Out through the avatar menu, which lands on the sign-in screen. */
async function signOut(target: Page = page): Promise<void> {
  await target.getByRole('button', { name: 'Your account' }).click();
  await target.getByRole('menuitem', { name: 'Sign out' }).click();
  await expect(target.getByRole('heading', { name: 'Sign in to Drive' })).toBeVisible();
}
