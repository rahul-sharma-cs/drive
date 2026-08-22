import { X, type LucideIcon } from 'lucide-react'
import { useEffect, useRef, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'

import type { DriveNode } from '../../lib/api'

/**
 * The row of commands above the list.
 *
 * It is always mounted, at a fixed height, in every state the list can be in —
 * loading, empty, failed, full. That is the whole point of it: a toolbar that
 * appears when something is selected pushes the list down by its own height,
 * and the row you clicked walks out from under the pointer just as you go to
 * click the next one. Here the two states are absolutely-positioned layers
 * crossfading in place, so nothing below ever moves.
 *
 * The inactive layer is `aria-hidden` and `visibility: hidden` rather than
 * `inert`: jsdom has no `inert` and Safari's support is uneven, while these two
 * take it out of the accessibility tree and the tab order in every browser the
 * app runs in. Opacity alone would leave a "Trash" button a screen reader could
 * still find and a pointer could still hit.
 *
 * It sits OUTSIDE the list card on purpose — the card is `overflow-hidden`,
 * which makes any `sticky` inside it inert.
 */

/**
 * One command in the selected layer.
 *
 * The band knows how to draw these and nothing about what they mean: a folder
 * offers Rename and Move to, the trash offers Restore and Delete forever, and
 * neither list belongs in here. Same shape as a row's `Action`, deliberately —
 * the two menus of commands on a screen should not describe themselves
 * differently.
 */
export interface BandAction {
  label: string
  icon: LucideIcon
  onSelect?: () => void
  /** A navigation — Download is a link, never a fetch. Excludes `onSelect`. */
  href?: string
  disabled?: boolean
  /** Red, and last: the one that throws something away. */
  danger?: boolean
}

export interface CommandBandProps {
  /** How many rows are loaded, for the idle layer's count. */
  count: number
  /** The selected rows, in list order. Empty means the idle layer is showing. */
  chosen: readonly DriveNode[]
  onClear: () => void
  /**
   * Where focus goes when the layer holding it is hidden. Clearing or spending
   * a selection leaves the button that was clicked focused inside a subtree
   * that is about to be `aria-hidden` — Chrome refuses to hide it and a screen
   * reader is left pointing at nothing, so the list takes focus back.
   */
  onReturnFocus?: () => void
  /** What the selected layer offers for these rows. */
  actions: (chosen: readonly DriveNode[]) => BandAction[]
  /** Sits beside the count in the idle layer — the trash's Empty trash. */
  idle?: ReactNode
}

export function CommandBand({ count, chosen, onClear, onReturnFocus, actions, idle }: CommandBandProps) {
  const selecting = chosen.length > 0

  // The count the selected layer shows is held over its fade-out, so the last
  // thing seen on the way out is "2 selected" rather than a flash of "0".
  const lastCount = useRef(0)
  if (selecting) lastCount.current = chosen.length

  // Held over the fade-out too, and for a better reason: recomputing the list
  // from an empty selection would blank the buttons out from under the fade.
  const lastActions = useRef<BandAction[]>([])
  if (selecting) lastActions.current = actions(chosen)

  return (
    // The wrapper carries the gap below the band as padding rather than leaving
    // it to a flex `gap`: the sticky box has to cover every pixel between the
    // chrome and the top of the card, or rows scroll through the seam.
    <div data-testid="command-band" className="sticky top-14 z-20 bg-canvas pt-1 pb-3">
      <div className="relative h-12">
        <Layer active={!selecting}>
          <span className="numeric px-2 text-ink-3">
            {count} {count === 1 ? 'item' : 'items'}
          </span>
          {/* Sorting is not here: it is on the column headers, where the thing
              being sorted is. Two controls for one setting is one too many. */}
          {idle}
        </Layer>

        <Layer active={selecting} onDeactivate={onReturnFocus}>
          <div
            role="toolbar"
            aria-label="Selection actions"
            // Never clipped: below `sm` these collapse to icons and the row
            // scrolls sideways instead of wrapping onto a second line, which is
            // the very thing that used to shove the list down.
            className="flex w-full items-center gap-1 overflow-x-auto rounded-card bg-teal-soft/60 px-1.5"
          >
            <Button variant="ghost" size="icon-sm" aria-label="Clear the selection" onClick={onClear}>
              <X />
            </Button>
            <span className="numeric shrink-0 px-1 text-teal-strong">{lastCount.current} selected</span>
            <span aria-hidden className="mx-1 h-4 w-px shrink-0 bg-teal/25" />

            {lastActions.current.map((action) => (
              <BandButton key={action.label} action={action} />
            ))}
          </div>
        </Layer>
      </div>
    </div>
  )
}

function Layer({
  active,
  onDeactivate,
  children,
}: {
  active: boolean
  onDeactivate?: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  const wasActive = useRef(active)
  useEffect(() => {
    const holdsFocus = ref.current?.contains(document.activeElement) ?? false
    if (wasActive.current && !active && holdsFocus) onDeactivate?.()
    wasActive.current = active
  }, [active, onDeactivate])

  return (
    <div
      ref={ref}
      aria-hidden={!active}
      // `visibility` is inline rather than a utility because it is the half of
      // this that a stylesheet-less environment still honours, and because the
      // pair (aria-hidden + visibility) has to move together.
      style={{ visibility: active ? 'visible' : 'hidden' }}
      className={`absolute inset-0 flex items-center transition-opacity duration-150 ease-out ${
        active ? 'opacity-100' : 'opacity-0'
      }`}
    >
      {children}
    </div>
  )
}

/**
 * A command in the band. The word is hidden below `sm` and the icon carries it,
 * but the accessible name is the same at every width — a control that loses its
 * name on a narrow screen loses it on the screen that needs it most.
 */
function BandButton({ action }: { action: BandAction }) {
  const Icon = action.icon
  const body = (
    <>
      <Icon />
      <span className="hidden sm:inline">{action.label}</span>
    </>
  )

  return (
    <Button
      variant="ghost"
      size="sm"
      asChild={action.href !== undefined}
      aria-label={action.label}
      disabled={action.disabled}
      onClick={action.onSelect}
      className={`shrink-0 ${action.danger ? 'hover:bg-danger-soft hover:text-danger' : ''}`}
    >
      {action.href === undefined ? (
        body
      ) : (
        // A download is a navigation to the 302 the API answers with, in its
        // own tab: the bytes come from the object store and must never pass
        // through this app, and a 401 rendered in this tab would replace it.
        <a href={action.href} target="_blank" rel="noopener">
          {body}
        </a>
      )}
    </Button>
  )
}
