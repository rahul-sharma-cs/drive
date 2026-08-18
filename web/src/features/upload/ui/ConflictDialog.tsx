import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { buttonClass, secondaryButtonClass } from '../../../ui/controls'
import type { ConflictPolicy, UploadSnapshot } from '../engine/types'

/**
 * One prompt at a time, for the first conflicted upload.
 *
 * PLAN §Conflict rules: bulk drops never prompt per item — folders reuse, and
 * file collisions get ONE prompt with apply-to-all, defaulting to keep-both.
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
        <Dialog.Overlay className="fixed inset-0 bg-neutral-900/20" />
        <Dialog.Content className="fixed left-1/2 top-1/3 w-96 -translate-x-1/2 rounded-xl bg-white p-5 shadow-lg">
          <Dialog.Title className="text-base font-medium">“{current.original_name}” is already here</Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-neutral-600">
            Keep both and this upload is saved under a new name. Replace and the file that is there now goes to the
            trash.
          </Dialog.Description>

          {others > 0 && (
            <label className="mt-3 flex items-center gap-2 text-sm">
              <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
              Do this for the other {others} {others === 1 ? 'file' : 'files'} too
            </label>
          )}

          <div className="mt-4 flex justify-end gap-2">
            <button className={secondaryButtonClass} onClick={() => onSkip(targets())}>
              Skip
            </button>
            <button className={secondaryButtonClass} onClick={() => onResolve(targets(), 'replace')}>
              Replace
            </button>
            <button className={buttonClass} onClick={() => onResolve(targets(), 'rename')}>
              Keep both
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
