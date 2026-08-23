import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

import {
  ApiError,
  createShare,
  regenerateShare,
  revokeShare,
  updateShareSettings,
  type Share,
  type ShareSettings,
  type ShareSettingsPatch,
} from '../../lib/api'
import { FormError } from '../../ui/controls'
import { sharesKey } from './queries'
import { shareUrls } from './shareUrls'

/**
 * The share mutations, and the two questions that stand in front of the
 * destructive ones.
 *
 * Both the dialog and `/shared` offer New link and Stop sharing, and neither is
 * undoable — a replaced link is one the owner can no longer even read — so the
 * confirms live here once, in the confirm-then-toast shape `IdentitiesSection`
 * uses, and the screens render `dialogs` wherever suits them.
 *
 * Every mutation invalidates the `['shares']` prefix, never an exact key: the
 * dialog reads `['shares','node',id]` and the list reads `['shares']`, and a
 * revoke that refreshed one but not the other would leave a dead link on
 * screen offering Copy.
 */

function useInvalidateShares() {
  const client = useQueryClient()
  return () => void client.invalidateQueries({ queryKey: sharesKey })
}

export function useCreateShare() {
  const invalidate = useInvalidateShares()
  return useMutation({
    mutationFn: ({ nodeId, settings }: { nodeId: string; settings: ShareSettings }) => createShare(nodeId, settings),
    onSuccess: ({ share, url }) => {
      shareUrls.set(share.id, url)
      invalidate()
    },
  })
}

export function useRegenerateShare() {
  const invalidate = useInvalidateShares()
  return useMutation({
    mutationFn: (id: string) => regenerateShare(id),
    onSuccess: ({ share, url }) => {
      shareUrls.set(share.id, url)
      invalidate()
    },
  })
}

export function useUpdateShareSettings() {
  const invalidate = useInvalidateShares()
  return useMutation({
    mutationFn: ({ id, settings }: { id: string; settings: ShareSettingsPatch }) => updateShareSettings(id, settings),
    onSuccess: invalidate,
  })
}

export function useRevokeShare() {
  const invalidate = useInvalidateShares()
  return useMutation({
    mutationFn: (id: string) => revokeShare(id),
    onSuccess: invalidate,
  })
}

/**
 * Puts a URL on the clipboard and says so — only once it is actually there.
 *
 * False means the person has to do it by hand: the API is absent (plain http
 * on a LAN is not a secure context) or the write was refused. The toast is
 * chained on the promise and never fired ahead of it, because a "copied" that
 * was not is the one failure that loses the link for good.
 */
export async function copyShareUrl(url: string): Promise<boolean> {
  const clipboard = typeof navigator === 'undefined' ? undefined : navigator.clipboard
  if (!clipboard || typeof clipboard.writeText !== 'function') return false
  try {
    await clipboard.writeText(url)
  } catch {
    return false
  }
  toast.success('Link copied')
  return true
}

export interface ShareCommands {
  /** Asks first; on yes, a new token and a zeroed count for the same file. */
  newLink: (share: Share) => void
  /** Asks first; on yes, the link stops and the row leaves every list. */
  stopSharing: (share: Share) => void
  /** One of the two is in flight. */
  busy: boolean
  /** The two confirms. The screen renders this somewhere. */
  dialogs: ReactNode
}

type Confirm = { kind: 'regenerate' | 'revoke'; share: Share } | null

export function useShareCommands(): ShareCommands {
  const regenerate = useRegenerateShare()
  const revoke = useRevokeShare()
  const invalidate = useInvalidateShares()
  const [confirm, setConfirm] = useState<Confirm>(null)

  const close = () => {
    regenerate.reset()
    revoke.reset()
    setConfirm(null)
  }

  /**
   * A 404 is the link having gone between the list being read and the click —
   * stopped in another tab, or the file purged. The question is about a row
   * that no longer exists, so it goes, and the lists re-read.
   */
  const gone = (err: unknown) => {
    if (!(err instanceof ApiError) || err.status !== 404) return
    invalidate()
    close()
    toast.success('That link was already turned off')
  }

  const dialogs = (
    <Dialog
      open={confirm !== null}
      onOpenChange={(open) => {
        if (!open) close()
      }}
    >
      <DialogContent className="sm:max-w-md">
        {confirm?.kind === 'regenerate' && (
          <>
            <DialogHeader>
              <DialogTitle>Make a new link?</DialogTitle>
              <DialogDescription>
                The current link stops working, and the download count starts again at zero.
              </DialogDescription>
            </DialogHeader>
            <FormError error={regenerate.error} />
            <DialogFooter>
              <Button variant="outline" onClick={close}>
                Cancel
              </Button>
              <Button
                disabled={regenerate.isPending}
                onClick={() =>
                  regenerate.mutate(confirm.share.id, {
                    onSuccess: () => {
                      close()
                      toast.success('New link made — copy it now')
                    },
                    onError: gone,
                  })
                }
              >
                {regenerate.isPending ? 'Making…' : 'New link'}
              </Button>
            </DialogFooter>
          </>
        )}

        {confirm?.kind === 'revoke' && (
          <>
            <DialogHeader>
              <DialogTitle>Stop sharing?</DialogTitle>
              <DialogDescription>Anyone with the link loses access; downloads already started finish.</DialogDescription>
            </DialogHeader>
            <FormError error={revoke.error} />
            <DialogFooter>
              <Button variant="outline" onClick={close}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={revoke.isPending}
                onClick={() =>
                  revoke.mutate(confirm.share.id, {
                    onSuccess: () => {
                      close()
                      toast.success('Sharing stopped')
                    },
                    onError: gone,
                  })
                }
              >
                {revoke.isPending ? 'Stopping…' : 'Stop sharing'}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )

  return {
    newLink: (share) => setConfirm({ kind: 'regenerate', share }),
    stopSharing: (share) => setConfirm({ kind: 'revoke', share }),
    busy: regenerate.isPending || revoke.isPending,
    dialogs,
  }
}
