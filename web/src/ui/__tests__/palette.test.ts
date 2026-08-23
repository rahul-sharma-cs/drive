// The sources are read as text, so this one needs no DOM.

import { describe, expect, it } from 'vitest'

/**
 * One palette in the product, asserted over the source rather than over a
 * single table.
 *
 * Drive's colours are declared as tokens in `index.css` — the three UI hues,
 * the closed set of type hues, three levels of ink, the surfaces — and the
 * whole reason they are declared is that a screen built out of them reads as
 * one system. A numbered shade out of Tailwind's own palette does not
 * participate in that: it is a colour from somewhere else that happens to look
 * near enough in the one place it was written, and the way it arrives is
 * always the same — a screen written quickly, or a component pasted in.
 *
 * Two of them survived a design pass that thought it had removed them all,
 * which is what this is here to stop happening again.
 *
 * `src/components/ui/` is exempt: those files are generated, they are the one
 * place a `dark:` utility or an upstream default is allowed to sit untouched,
 * and the variable block at the foot of `index.css` is what points their
 * semantic names — background, primary, border — at the tokens above.
 */

const sources = import.meta.glob('../../**/*.{ts,tsx,css}', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

const STOCK_SHADE =
  /-(red|blue|sky|violet|rose|orange|emerald|indigo|slate|zinc|neutral|gray|amber|yellow|green|teal|cyan|pink|purple|fuchsia|lime)-[0-9]/

describe('the product palette', () => {
  it('reads every source file this rule covers', () => {
    // Without this the glob could match nothing at all — a moved directory, a
    // pattern that stopped resolving — and the assertion below would pass over
    // an empty set, which is the failure mode a guard like this dies of.
    const scanned = Object.keys(sources).filter((path) => !path.includes('/components/ui/'))
    expect(scanned.length).toBeGreaterThan(60)
    expect(scanned.some((path) => path.endsWith('/index.css'))).toBe(true)
  })

  it('never spends a numbered shade out of a stock palette', () => {
    const offenders: string[] = []
    for (const [path, source] of Object.entries(sources)) {
      if (path.includes('/components/ui/')) continue
      source.split('\n').forEach((line, i) => {
        if (STOCK_SHADE.test(line)) offenders.push(`${path}:${i + 1}: ${line.trim()}`)
      })
    }
    expect(offenders).toEqual([])
  })
})
