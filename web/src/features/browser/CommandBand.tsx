import { ArrowUp, Copy, Download, FolderInput, Pencil, Trash2, X } from 'lucide-react'
import { useRef, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'

import { downloadHref, type DriveNode } from '../../lib/api'

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

/** What the selected layer's commands need from the screen holding the dialogs. */
export interface BandCommands {
  onRename: (node: DriveNode) => void
  onCopy: (nodes: DriveNode[]) => void
  onMove: (nodes: DriveNode[]) => void
  onTrash: (nodes: DriveNode[]) => void
}

export interface CommandBandProps {
  /** How many rows are loaded, for the idle layer's count. */
  count: number
  /** The selected rows, in list order. Empty means the idle layer is showing. */
  chosen: readonly DriveNode[]
  /** A mutation is in flight — the commands that would start another are off. */
  busy?: boolean
  onClear: () => void
  commands: BandCommands
}

export function CommandBand({ count, chosen, busy = false, onClear, commands }: CommandBandProps) {
  const selecting = chosen.length > 0

  // The count the selected layer shows is held over its fade-out, so the last
  // thing seen on the way out is "2 selected" rather than a flash of "0".
  const lastCount = useRef(0)
  if (selecting) lastCount.current = chosen.length

  const single = chosen.length === 1 ? chosen[0] : null
  const files = chosen.filter((n) => n.kind === 'file')

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
          {/* Sorting is a server parameter that does not exist yet. The chips
              are here because their arrival must not be the thing that finally
              moves this row — they are disabled until the API takes `sort`. */}
          <span aria-hidden className="mx-1 h-4 w-px bg-line" />
          {(['Name', 'Modified', 'Size'] as const).map((by) => (
            <Button key={by} variant="ghost" size="sm" disabled className="text-ink-3">
              {by}
              {by === 'Name' && <ArrowUp className="opacity-60" />}
            </Button>
          ))}
        </Layer>

        <Layer active={selecting}>
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

            {/* One file at a time: there is no archive endpoint to answer a
                multi-selection with, and a button that silently downloaded one
                of five would be a lie. It takes the whole selection once
                downloading a set of files as one zip exists. */}
            {single?.kind === 'file' && (
              <BandButton asChild label="Download" icon={Download}>
                <a href={downloadHref(single.id)} target="_blank" rel="noopener">
                  <Download />
                  <Label>Download</Label>
                </a>
              </BandButton>
            )}
            {single && (
              <BandButton label="Rename" icon={Pencil} disabled={busy} onClick={() => commands.onRename(single)} />
            )}
            <BandButton
              label="Move to"
              icon={FolderInput}
              disabled={busy}
              onClick={() => commands.onMove([...chosen])}
            />
            {/* Files only: the server answers a folder copy with 422. */}
            {files.length > 0 && (
              <BandButton label="Copy to" icon={Copy} disabled={busy} onClick={() => commands.onCopy(files)} />
            )}
            <BandButton
              label="Trash"
              icon={Trash2}
              disabled={busy}
              danger
              onClick={() => commands.onTrash([...chosen])}
            />
          </div>
        </Layer>
      </div>
    </div>
  )
}

function Layer({ active, children }: { active: boolean; children: ReactNode }) {
  return (
    <div
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
function BandButton({
  label,
  icon: Icon,
  danger,
  asChild,
  children,
  ...props
}: {
  label: string
  icon: typeof Download
  danger?: boolean
  asChild?: boolean
  children?: ReactNode
  disabled?: boolean
  onClick?: () => void
}) {
  return (
    <Button
      variant="ghost"
      size="sm"
      asChild={asChild}
      aria-label={label}
      className={`shrink-0 ${danger ? 'hover:bg-danger-soft hover:text-danger' : ''}`}
      {...props}
    >
      {children ?? (
        <>
          <Icon />
          <Label>{label}</Label>
        </>
      )}
    </Button>
  )
}

function Label({ children }: { children: ReactNode }) {
  return <span className="hidden sm:inline">{children}</span>
}
