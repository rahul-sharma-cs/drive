import { describe, expect, it } from 'vitest'

import { formatUntil, formatWhen } from '../when'

const now = new Date('2026-08-23T12:00:00Z')
const ahead = (ms: number) => new Date(now.getTime() + ms).toISOString()
const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR

/**
 * The expiry formatter exists because the elapsed-time one answers "Just now"
 * to every future instant — which is what a 7-day link would have read as.
 */
describe('formatUntil', () => {
  it('reads a week ahead as a week, where formatWhen reads it as Just now', () => {
    const week = ahead(7 * DAY)
    expect(formatWhen(week, now)).toBe('Just now')
    expect(formatUntil(week, now)).toBe('Expires in 7 days')
  })

  it('reads hours ahead in hours', () => {
    expect(formatUntil(ahead(HOUR), now)).toBe('Expires in 1 hour')
    expect(formatUntil(ahead(3 * HOUR), now)).toBe('Expires in 3 hours')
  })

  it('reads a past instant as Expired', () => {
    expect(formatUntil(ahead(-1000), now)).toBe('Expired')
    expect(formatUntil(ahead(-3 * DAY), now)).toBe('Expired')
  })

  it('reads null as Expires never', () => {
    expect(formatUntil(null, now)).toBe('Expires never')
  })

  it('reads a day ahead as tomorrow, and past a month gives the date itself', () => {
    expect(formatUntil(ahead(DAY), now)).toBe('Expires tomorrow')
    expect(formatUntil(ahead(6 * DAY + 23 * HOUR), now)).toBe('Expires in 7 days')
    // Locale-formatted, so the shape is pinned rather than the string: a date,
    // not a count.
    expect(formatUntil(ahead(45 * DAY), now)).toMatch(/^Expires (?!in )\S/)
  })
})
