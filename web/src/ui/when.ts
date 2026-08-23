/**
 * Timestamps as a person reads them: recent things by clock, this year by day,
 * older things with the year attached. A file list is scanned, not read, so
 * the useful part is how long ago — never a full ISO string.
 */
export function formatWhen(iso: string, now: Date = new Date()): string {
  const then = new Date(iso)
  if (Number.isNaN(then.getTime())) return ''

  const minutes = Math.floor((now.getTime() - then.getTime()) / 60000)
  if (minutes < 1) return 'Just now'
  if (minutes < 60) return `${minutes}m ago`
  if (minutes < 24 * 60) return `${Math.floor(minutes / 60)}h ago`

  return dayOf(then, now)
}

/**
 * The other direction: when something stops. `formatWhen` is elapsed time
 * only — handed a future instant it answers "Just now" — so an expiry goes
 * through here instead. A whole clause, because the three shapes it takes do
 * not share a prefix: "Expires in 6 days", "Expired", "Expires never".
 */
export function formatUntil(iso: string | null, now: Date = new Date()): string {
  if (iso === null) return 'Expires never'
  const then = new Date(iso)
  if (Number.isNaN(then.getTime())) return ''

  const minutes = Math.floor((then.getTime() - now.getTime()) / 60000)
  if (minutes < 0) return 'Expired'
  if (minutes < 1) return 'Expires in a minute'
  if (minutes < 60) return `Expires in ${plural(minutes, 'minute')}`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `Expires in ${plural(hours, 'hour')}`
  const days = Math.round(minutes / (24 * 60))
  if (days <= 1) return 'Expires tomorrow'
  if (days <= 30) return `Expires in ${days} days`
  return `Expires ${dayOf(then, now)}`
}

const plural = (n: number, unit: string) => `${n} ${unit}${n === 1 ? '' : 's'}`

function dayOf(then: Date, now: Date): string {
  const sameYear = then.getFullYear() === now.getFullYear()
  return then.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
  })
}
