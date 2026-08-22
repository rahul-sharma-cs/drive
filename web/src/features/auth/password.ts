/**
 * The client-side mirror of the server's own password rule.
 *
 * The server checks a length and nothing else — no character classes, no
 * strength score — so this checks a length and nothing else too. Saying more
 * here than the API enforces would invent a rule the product does not have.
 *
 * It counts UTF-8 bytes because the server counts UTF-8 bytes (Go's `len` over
 * a string). For an ordinary password the two are the same number; where they
 * differ, counting anything else here would refuse a password the API would
 * have taken, which is the one direction a client-side check must never fail
 * in.
 */
export const minPasswordLen = 8
export const maxPasswordLen = 128

const utf8 = new TextEncoder()

export function passwordLength(value: string): number {
  return utf8.encode(value).length
}

export function isAcceptablePassword(value: string): boolean {
  const n = passwordLength(value)
  return n >= minPasswordLen && n <= maxPasswordLen
}

/**
 * The line under the field. It is always on screen, quiet, so the rule is known
 * before it is broken rather than after — the field only turns red once the
 * value has actually been judged.
 */
export function passwordHint(value: string): string {
  return passwordLength(value) > maxPasswordLen
    ? `At most ${maxPasswordLen} characters`
    : `At least ${minPasswordLen} characters`
}
