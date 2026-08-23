import { expect, type APIRequestContext } from '@playwright/test';
import { readFileSync } from 'node:fs';

/**
 * The Mailpit inbox two specs read, and the `.env.test` reader that points them
 * at it.
 *
 * The account screens prove the reset loop, and the Google flow proves that a
 * Google-only account can add a password through that same loop, so both files
 * wait on the same mailbox. A second copy of a thirty-second poll is a second
 * thing to keep true, and the two would drift the first time one of them was
 * tightened.
 *
 * It sits under `tests/` so the imports stay short, in a directory playwright
 * does not collect: a run matches `*.spec.ts`, and this is not one.
 *
 * The request context arrives as a getter rather than a value. A spec builds its
 * Mailpit context in beforeAll, and there is no context to hand over at the
 * point where these helpers are bound.
 */
export function mailbox(context: () => APIRequestContext, base: string) {
  /** Every message addressed to `addr`, newest first. */
  async function inboxTo(addr: string): Promise<MailSummary[]> {
    // Newest first, and 200 is far more than one run of this file sends, so the
    // window never closes over a message it is waiting for.
    const res = await context().get('/api/v1/messages?limit=200');
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
        const res = await context().get(`/api/v1/message/${encodeURIComponent(match.ID)}`);
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
    return `${base}${pathQuery}${found![1]}`;
  }

  // inboxTo stays in here: it is how the two waits are written, not something a
  // spec has ever needed to ask for.
  return { waitForMailCount, waitForMailTo, linkFrom };
}

/** Mailpit's inbox shapes, cut down to what these files read. */
export type MailSummary = { ID: string; To: { Address: string }[]; Subject: string };
export type MailMessage = MailSummary & { Text: string };

const MAIL_DEADLINE = 30_000;
const MAIL_POLL = 200;

export function readEnvFile(path: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of readFileSync(path, 'utf8').split('\n')) {
    const trimmed = line.trim();
    if (trimmed === '' || trimmed.startsWith('#')) continue;
    const at = trimmed.indexOf('=');
    out[trimmed.slice(0, at)] = trimmed.slice(at + 1);
  }
  return out;
}
