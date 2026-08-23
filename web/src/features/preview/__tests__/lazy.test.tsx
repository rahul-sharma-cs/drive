// @vitest-environment jsdom

import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { describe, expect, it, vi } from 'vitest'

import { PreviewDialog } from '../PreviewDialog.lazy'

/**
 * The boundary, with the chunk held open.
 *
 * The viewer is split out of the first bundle and warmed on idle, so in the
 * ordinary case the click and the panel are the same moment. This file is
 * about the case where they are not — a click inside the first second, a cold
 * cache, a slow connection — because that is the case where the fallback is
 * the entire answer, and `null` is not one: the page sits there unchanged
 * while a request goes out, which reads as a click that did not register.
 *
 * The gate is what makes the wait real rather than a race: the mocked module
 * does not resolve until the test lets it, so "immediately after the open" is
 * a state the assertions can stand still in.
 */
const gate = vi.hoisted(() => {
  let open!: () => void
  const held = new Promise<void>((resolve) => {
    open = resolve
  })
  return { held, open: () => open() }
})

vi.mock('../PreviewDialog', async () => {
  await gate.held
  return { default: () => <div data-testid="viewer">the viewer</div> }
})

describe('the viewer boundary', () => {
  it('acknowledges the click with the viewer’s own scrim before the chunk lands', async () => {
    render(
      <MemoryRouter initialEntries={['/?preview=f2']}>
        <PreviewDialog nodes={[]} />
      </MemoryRouter>,
    )

    // Same frame as the open. The panel is not here yet and cannot be — the
    // module it lives in has not resolved.
    expect(screen.queryByTestId('viewer')).toBeNull()
    const scrim = document.querySelector('.scrim')
    expect(scrim).toBeTruthy()
    // The dimmed page is the viewer opening, not a message about it.
    expect(scrim!.getAttribute('aria-hidden')).toBe('true')

    gate.open()
    await waitFor(() => expect(screen.getByTestId('viewer')).toBeTruthy())
    // And it hands over rather than stacking: the real dialog draws its own.
    expect(document.querySelector('.scrim')).toBeNull()
  })
})
