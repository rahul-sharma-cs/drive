import { describe, expect, it } from 'vitest'

import { emailHint, isPlausibleEmail } from '../email'

describe('isPlausibleEmail', () => {
  it.each([
    // The case that started this: a domain with no dot in it parses as a valid
    // address in every strict parser, and is never what anybody meant to type.
    ['a@b', false],
    ['wfwef@fweffwef', false],
    ['a@b.co', true],
    ['wfwef@fweffwef.example', true],
    // A final label of one character is a typo, not a TLD.
    ['a@b.c', false],
    ['a b@c.d', false],
    ['a@@b.c', false],
    // Trimmed before it is judged: an address pasted out of a mail client
    // arrives with whitespace around it often enough.
    [' a@b.com ', true],
    ['', false],
    ['@b.com', false],
    ['a@', false],
    ['a@.com', false],
    ['a@b..com', false],
    ['a.b+tag@sub.domain.example', true],
    ['A@B.Example', true],
    ['a@b-c.example', true],
    // The final label has to be letters, so a stray digit reads as the typo it
    // usually is.
    ['a@b.c0', false],
    // Except for an IDN TLD in its ASCII form, which is letters, digits and a
    // hyphen by construction — xn--p1ai is .рф.
    ['a@example.xn--p1ai', true],
    ['A@EXAMPLE.XN--P1AI', true],
    // The prefix on its own is not a TLD.
    ['a@b.xn--', false],
    // Whitespace only survives at the ends, where trimming takes it.
    ['a@b.com\r\nBcc: evil@x.example', false],
  ])('%j -> %s', (value, want) => {
    expect(isPlausibleEmail(value)).toBe(want)
  })
})

describe('emailHint', () => {
  it('tells an empty field it is empty, not that it is malformed', () => {
    // The forms carry noValidate, so the browser's "please fill out this
    // field" is gone and this line is the only thing left saying so.
    expect(emailHint('   ')).toBe('Enter an email address')
  })

  it('points at the half that is usually wrong', () => {
    expect(emailHint('wfwef@fweffwef')).toMatch(/after the @/)
  })
})
