/**
 * Exponential backoff with jitter, capped at 60 s per wait. The cap matters:
 * an upload can run for hours, so retries have to stay patient rather than
 * escalating into waits nobody would sit through.
 *
 * The RNG is injected so the vitest suite is deterministic.
 */

export const BACKOFF_BASE_MS = 1_000
export const BACKOFF_CAP_MS = 60_000

/**
 * `attempt` is 1-based. Returns a delay in [exp/2, exp] where
 * `exp = min(cap, base * 2^(attempt-1))` — full-ish jitter, never above the cap.
 */
export function backoffDelay(
  attempt: number,
  random: () => number,
  baseMs = BACKOFF_BASE_MS,
  capMs = BACKOFF_CAP_MS,
): number {
  const n = Math.max(1, Math.floor(attempt))
  // 2^30 * base already exceeds any cap; clamp the exponent so it can't overflow.
  const exp = Math.min(capMs, baseMs * 2 ** Math.min(n - 1, 30))
  return Math.round(exp / 2 + random() * (exp / 2))
}
