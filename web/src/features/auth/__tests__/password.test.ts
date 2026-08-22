import { describe, expect, it } from 'vitest'

import { isAcceptablePassword, maxPasswordLen, minPasswordLen, passwordHint } from '../password'

describe('isAcceptablePassword', () => {
  it('mirrors the server: a length, and nothing else', () => {
    expect(isAcceptablePassword('')).toBe(false)
    expect(isAcceptablePassword('short')).toBe(false)
    expect(isAcceptablePassword('1234567')).toBe(false)
    expect(isAcceptablePassword('12345678')).toBe(true)
    expect(isAcceptablePassword('x'.repeat(maxPasswordLen))).toBe(true)
    expect(isAcceptablePassword('x'.repeat(maxPasswordLen + 1))).toBe(false)
    // No character classes, because the server has none. Inventing one here
    // would refuse a password the API would have taken.
    expect(isAcceptablePassword('aaaaaaaa')).toBe(true)
    expect(isAcceptablePassword('        ')).toBe(true)
  })

  it('counts UTF-8 bytes, the way the server does', () => {
    // Go's len() over a string counts bytes, so four two-byte characters are
    // eight to the server. Counting JS string length here would refuse this.
    const fourTwoByteChars = 'éééé'
    expect(fourTwoByteChars.length).toBe(4)
    expect(isAcceptablePassword(fourTwoByteChars)).toBe(true)
  })
})

describe('passwordHint', () => {
  it('states the rule before it is broken', () => {
    expect(passwordHint('')).toBe(`At least ${minPasswordLen} characters`)
    expect(passwordHint('short')).toBe(`At least ${minPasswordLen} characters`)
  })

  it('names the other end when that is the one being hit', () => {
    expect(passwordHint('x'.repeat(maxPasswordLen + 1))).toBe(`At most ${maxPasswordLen} characters`)
  })
})
