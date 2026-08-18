import * as Dialog from '@radix-ui/react-dialog'
import { useState } from 'react'

import { buttonClass, fieldClass, FormError, inputClass, secondaryButtonClass } from '../../ui/controls'
import { useCreateFolder } from './queries'

/** Folder-with-a-plus: the one glyph the icon set needs only here. */
function FolderPlusIcon() {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className="h-4 w-4"
    >
      <path d="M1.75 4.25c0-.55.45-1 1-1h3.09c.3 0 .58.13.77.36l.78.93h5.86c.55 0 1 .45 1 1v6.2c0 .55-.45 1-1 1H2.75c-.55 0-1-.45-1-1z" />
      <path d="M8 7.6v3.4M6.3 9.3h3.4" />
    </svg>
  )
}

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
      <Dialog.Trigger className={secondaryButtonClass}>
        <FolderPlusIcon />
        New folder
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-[42%] z-50 w-[min(20rem,calc(100vw-2rem))] rounded-pop border border-line bg-surface p-5 shadow-pop">
          <Dialog.Title className="text-[15px] font-semibold">New folder</Dialog.Title>
          <Dialog.Description className="sr-only">Create a folder in the current folder.</Dialog.Description>
          <form
            className="mt-4 flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(name, { onSuccess: () => setOpen(false) })
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
