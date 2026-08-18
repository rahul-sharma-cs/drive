import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { buttonClass, fieldClass, FormError, inputClass, secondaryButtonClass } from '../../ui/controls'

/**
 * Rename. The input opens with the current name selected up to the extension,
 * which is the thing being changed nine times out of ten.
 */
export function RenameDialog({
  currentName,
  error,
  busy,
  onRename,
  onCancel,
}: {
  currentName: string
  error: unknown
  busy: boolean
  onRename: (name: string) => void
  onCancel: () => void
}) {
  const [name, setName] = useState(currentName)

  return (
    <Dialog.Root open onOpenChange={(next) => !next && onCancel()}>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-1/2 z-50 w-[min(22rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-pop border border-line bg-surface p-5 shadow-pop">
          <Dialog.Title className="text-[15px] font-semibold">Rename</Dialog.Title>
          <Dialog.Description className="sr-only">Give this item a new name.</Dialog.Description>
          <form
            className="mt-4 flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              onRename(name.trim())
            }}
          >
            <label className={fieldClass}>
              Name
              <input
                className={inputClass}
                value={name}
                required
                autoFocus
                onChange={(e) => setName(e.target.value)}
                onFocus={(e) => {
                  const dot = e.target.value.lastIndexOf('.')
                  e.target.setSelectionRange(0, dot > 0 ? dot : e.target.value.length)
                }}
              />
            </label>
            <FormError error={error} />
            <div className="flex justify-end gap-2 pt-1">
              <button type="button" className={secondaryButtonClass} onClick={onCancel}>
                Cancel
              </button>
              <button className={buttonClass} type="submit" disabled={busy || name.trim() === ''}>
                Rename
              </button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
