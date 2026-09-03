// @vitest-environment jsdom

/**
 * The owner's own links, kept in the owner's own browser. The server stores a
 * hash and can never show a URL again, so what survives a reload is this map's
 * copy in `localStorage` — and a browser with no usable storage must cost
 * nothing but the reload.
 *
 * Each case takes a fresh module instance where it matters: the read happens
 * once, when the module loads, which is exactly what a reload does.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'

const KEY = 'drive.share-urls'
const URL_1 = 'https://drive.example/s/0123456789abcdef0123456789abcdef0123456789a'
const URL_2 = 'https://drive.example/s/fedcba9876543210fedcba9876543210fedcba98765'

/** What a reload gets: the module evaluated again, reading storage as it starts. */
async function fresh() {
  vi.resetModules()
  return (await import('../shareUrls')).shareUrls
}

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('the stored copy', () => {
  it('is written on every set, and a fresh instance reads it back', async () => {
    const before = await fresh()
    before.set('s1', URL_1)
    before.set('s2', URL_2)
    expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual({ s1: URL_1, s2: URL_2 })

    const reloaded = await fresh()
    expect(reloaded.get('s1')).toBe(URL_1)
    expect(reloaded.get('s2')).toBe(URL_2)
  })

  it('is merged on every write, so two tabs minting links keep both', async () => {
    // Both tabs open before either link exists, so each holds only its own.
    const tabA = await fresh()
    const tabB = await fresh()
    tabA.set('s1', URL_1)
    tabB.set('s2', URL_2)

    expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual({ s1: URL_1, s2: URL_2 })
    const reloaded = await fresh()
    expect(reloaded.get('s1')).toBe(URL_1)
    expect(reloaded.get('s2')).toBe(URL_2)
  })

  it('is removed by clear, so the next load holds nothing', async () => {
    const before = await fresh()
    before.set('s1', URL_1)
    before.clear()

    expect(localStorage.getItem(KEY)).toBeNull()
    expect(before.get('s1')).toBeUndefined()
    expect((await fresh()).get('s1')).toBeUndefined()
  })

  it('is not relied on: a storage that throws leaves set and get working in memory', async () => {
    const refuse = () => {
      throw new Error('storage is not available')
    }
    vi.stubGlobal('localStorage', { getItem: refuse, setItem: refuse, removeItem: refuse })

    const store = await fresh()
    store.set('s1', URL_1)
    expect(store.get('s1')).toBe(URL_1)
    store.clear()
    expect(store.get('s1')).toBeUndefined()
  })
})
