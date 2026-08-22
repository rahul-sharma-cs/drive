import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { buttonClass, fieldClass, FormError, inputClass, secondaryButtonClass } from '../../ui/controls'
import { useCreateFolder } from './queries'

/**
 * New Folder.
 *
 * Controlled from outside, with no trigger of its own: the command that opens
 * it lives in the rail's New menu, which closes as this opens, and a dialog
 * owned by a menu item would be unmounted mid-open along with it.
 *
 * Radix's modal Dialog sets `pointer-events: none` on <body> while it is open
 * and removes it on close. That is worth knowing about here because the page
 * behind this dialog is a drop target: a drop attempted while the dialog is
 * open is swallowed by design, and one attempted after it closes must work.
 * `shell.test.tsx` pins the second half.
 */
export function NewFolderDialog({
  parentId,
  open,
  onOpenChange,
}: {
  parentId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const [name, setName] = useState('')
  const create = useCreateFolder(parentId)

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next)
        if (!next) {
          setName('')
          create.reset()
        }
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-[42%] z-50 w-[min(20rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-pop border border-line bg-surface p-5 shadow-pop">
          <Dialog.Title className="text-[15px] font-semibold">New folder</Dialog.Title>
          <Dialog.Description className="sr-only">Create a folder in the current folder.</Dialog.Description>
          <form
            className="mt-4 flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(name, {
                onSuccess: () => {
                  onOpenChange(false)
                  setName('')
                  create.reset()
                },
              })
            }}
          >
            <label className={fieldClass}>
              Name
              <input className={inputClass} value={name} required onChange={(e) => setName(e.target.value)} autoFocus />
            </label>
            <FormError error={create.error} />
            <div className="flex justify-end gap-2 pt-1">
              <Dialog.Close className={secondaryButtonClass} type="button">
                Cancel
              </Dialog.Close>
              <button className={buttonClass} type="submit" disabled={create.isPending}>
                Create
              </button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
