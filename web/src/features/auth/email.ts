/**
 * The one shape test the sign-in, sign-up and reset screens agree on.
 *
 * Deliberately not RFC 5322. The job here is *feedback* — telling somebody who
 * typed `wfwef@fweffwef` that there is no such thing as that address, while
 * they are still looking at the field — and not gatekeeping, so the rule stays
 * loose enough that no ordinary mailbox trips it: trimmed, no whitespace,
 * exactly one `@`, a non-empty local part, and a domain of non-empty
 * dot-separated labels whose last one is two or more letters — or an IDN TLD in
 * its ASCII form, `xn--` and at least one character more, which is the one real
 * TLD family the letters-only rule would otherwise refuse. That last clause is
 * the whole point: `a@b` parses as an address in every strict parser there is,
 * and is still never what a person meant to type.
 *
 * What it knowingly refuses: a bare hostname domain (`root@localhost`) and an
 * address literal (`a@[192.0.2.1]`). Neither can reach a mailbox this product
 * would ever mail, and the server applies the same rule, so a value the field
 * accepts is a value the API accepts.
 */
const PLAUSIBLE = /^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)*\.(?:[A-Za-z]{2,}|[Xx][Nn]--[A-Za-z0-9-]+)$/

export function isPlausibleEmail(value: string): boolean {
  return PLAUSIBLE.test(value.trim())
}

/**
 * What to say under a field that failed the test.
 *
 * Empty gets its own line because the forms carry `noValidate` — the native
 * "please fill out this field" bubble is gone, and something has to take its
 * place or an empty submit does nothing at all.
 */
export function emailHint(value: string): string {
  return value.trim() === ''
    ? 'Enter an email address'
    : 'That doesn’t look like an email address — check the part after the @'
}

/**
 * The danger treatment for the field itself, layered onto `inputClass`. Driven
 * off `aria-invalid` rather than a conditional class so the assistive-tech
 * signal and the visible one cannot come apart.
 */
export const invalidEmailClass =
  'aria-invalid:border-danger aria-invalid:ring-2 aria-invalid:ring-danger/20 ' +
  'aria-invalid:focus:border-danger aria-invalid:focus:ring-danger/20'
