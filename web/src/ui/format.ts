/**
 * Byte counts as a person reads them.
 *
 * Decimal units, because that is what a storage product means by "GB" — the
 * number on the rail has to agree with the number in the head of someone who
 * has used Drive or Dropbox, not with the number in a memory allocator. The
 * server's refusal messages use the same base, so the meter and the error a
 * user hits when they exceed it never disagree.
 */
export function formatBytes(bytes: number): string {
  if (bytes < 1000) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1000
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000
    unit++
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
}
