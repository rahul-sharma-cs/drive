import { describe, expect, it } from 'vitest'

import { formatBytes } from '../format'

/**
 * The base is the whole point of this function, so it is what gets pinned.
 *
 * A storage product's "GB" is 10^9 — the number on the rail has to agree with
 * the server's refusal copy, which counts the same way. Switching this back to
 * 1024 renders a 3 GB quota as "2.8 GB" while the 422 still says 3 GB.
 */
describe('formatBytes', () => {
  it('divides by 1000, not 1024', () => {
    expect(formatBytes(1000)).toBe('1.0 KB')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(3_000_000_000)).toBe('3.0 GB')
  })

  it('shows bytes below a kilobyte, and drops the decimal past ten units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(999)).toBe('999 B')
    expect(formatBytes(420_000_000)).toBe('420 MB')
  })

  it('stops climbing at terabytes', () => {
    expect(formatBytes(5_000_000_000_000_000)).toBe('5000 TB')
  })
})
