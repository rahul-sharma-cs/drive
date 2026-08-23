import { CircleAlert } from 'lucide-react'
import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'

import type { ConflictPolicy, UploadSnapshot } from '../engine/types'

/**
 * One prompt at a time, for the first conflicted upload.
 *
 * Bulk drops never prompt per item — folders reuse an existing folder of the
 * same name, and file collisions get ONE prompt with apply-to-all, defaulting
 * to keep-both.
 * Rendering only the first conflicted row is what enforces the single slot: a
 * 150-file drop into a folder that already has those names would otherwise
 * stack 150 modals.
 *
 * There is no dismiss path, and that is structural rather than a set of
 * handlers: `open` is passed as a constant with no `onOpenChange`, so Radix's
 * escape/click-away route calls a no-op and the dialog closes only when the
 * upload leaves the `conflict` state — which happens because a button below
 * was pressed. The upload is blocked until then, and a prompt that could be
 * dismissed would leave a row looking stuck for no visible reason.
 */
export function ConflictDialog({
  conflicts,
  onResolve,
  onSkip,
}: {
  conflicts: UploadSnapshot[]
  onResolve: (ids: string[], policy: ConflictPolicy) => void
  onSkip: (ids: string[]) => void
}) {
  const [all, setAll] = useState(false)
  const current = conflicts[0]
  if (!current) return null

  const targets = () => (all ? conflicts.map((c) => c.id) : [current.id])
  const others = conflicts.length - 1

  return (
    <Dialog.Root open>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-[42%] z-50 w-[min(24rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-pop border border-line bg-surface p-5 shadow-pop">
          <span className="flex h-8 w-8 items-center justify-center rounded-full bg-warn-soft text-warn">
            <CircleAlert aria-hidden className="size-4" />
          </span>
          <Dialog.Title className="mt-3 text-[15px] leading-snug font-semibold">
            “{current.original_name}” is already here
          </Dialog.Title>
          <Dialog.Description className="mt-1.5 text-[13px] text-ink-2">
            Keep both and this upload is saved under a new name. Replace and the file that is there now goes to the
            trash.
          </Dialog.Description>

          {others > 0 && (
            <label className="mt-3 flex items-center gap-2 rounded-control bg-surface-muted px-3 py-2 text-[13px] text-ink-2">
              {/* The product's checkbox, the same one the file rows are
                  selected with — a bare `<input type="checkbox">` wearing an
                  `accent-` colour is the browser's box in Drive's teal, which
                  is a different shape and a different focus ring from every
                  other box on the screen. Still inside the <label>, so the
                  sentence beside it is the control's name and clicking the
                  words still toggles it. */}
              <Checkbox checked={all} onCheckedChange={(next) => setAll(next === true)} />
              Do this for the other {others} {others === 1 ? 'file' : 'files'} too
            </label>
          )}

          <div className="mt-4 flex flex-wrap justify-end gap-2">
            <Button variant="ghost" onClick={() => onSkip(targets())}>
              Skip
            </Button>
            <Button variant="outline" onClick={() => onResolve(targets(), 'replace')}>
              Replace
            </Button>
            <Button onClick={() => onResolve(targets(), 'rename')}>Keep both</Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
