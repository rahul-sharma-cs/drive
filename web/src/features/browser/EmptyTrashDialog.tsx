import * as Dialog from '@radix-ui/react-dialog'

import { Button } from '@/components/ui/button'

/**
 * The one confirmation in the app that cannot be walked back, so it says the
 * number out loud: "all 12 items" is the difference between emptying a trash
 * you looked at and emptying one you forgot you had filled.
 *
 * The number is only claimed when the whole trash is on screen. The route
 * empties everything, not just the loaded page, so with more pages waiting the
 * honest wording is the one that does not count.
 */
export function EmptyTrashDialog({
  count,
  exact,
  busy,
  onConfirm,
  onCancel,
}: {
  count: number
  /** Every trashed root is loaded, so `count` is the whole of it. */
  exact: boolean
  busy: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  const title = exact
    ? `Delete all ${count} ${count === 1 ? 'item' : 'items'} forever?`
    : 'Delete everything in the trash forever?'

  return (
    <Dialog.Root open onOpenChange={(next) => !next && onCancel()}>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim fixed inset-0 z-50" />
        <Dialog.Content className="pop-enter fixed left-1/2 top-[42%] z-50 w-[min(24rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-pop border border-line bg-surface p-5 shadow-pop">
          <Dialog.Title className="text-[15px] font-semibold">{title}</Dialog.Title>
          <Dialog.Description className="mt-2 text-[13px] text-ink-3">
            Folders in the trash go with everything inside them. This cannot be undone.
          </Dialog.Description>
          <div className="mt-5 flex justify-end gap-2">
            <Button variant="outline" onClick={onCancel}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={busy} onClick={onConfirm}>
              Empty trash
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
