import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { buttonClass, FormError, inputClass, secondaryButtonClass } from '../../ui/controls'
import { useCreateFolder } from './queries'

/**
 * New Folder.
 *
 * Radix's modal Dialog sets `pointer-events: none` on <body> while it is open
 * and removes it on close. That is worth knowing about here because the page
 * behind this dialog is a drop target: a drop attempted while the dialog is
 * open is swallowed by design, and one attempted after it closes must work.
 * `browser.test.tsx` pins the second half.
 */
export function NewFolderDialog({ parentId }: { parentId: string }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const create = useCreateFolder(parentId)

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) {
          setName('')
          create.reset()
        }
      }}
    >
      <Dialog.Trigger className={secondaryButtonClass}>New folder</Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-neutral-900/20" />
        <Dialog.Content className="fixed left-1/2 top-1/3 w-80 -translate-x-1/2 rounded-xl bg-white p-5 shadow-lg">
          <Dialog.Title className="text-base font-medium">New folder</Dialog.Title>
          <Dialog.Description className="sr-only">Create a folder in the current folder.</Dialog.Description>
          <form
            className="mt-4 flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(name, { onSuccess: () => setOpen(false) })
            }}
          >
            <label className="flex flex-col gap-1 text-sm">
              Name
              <input className={inputClass} value={name} required onChange={(e) => setName(e.target.value)} autoFocus />
            </label>
            <FormError error={create.error} />
            <div className="flex justify-end gap-2">
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
