import { Link2 } from 'lucide-react'
import { Link } from 'react-router'

import { Button } from '@/components/ui/button'

import type { Share } from '../../lib/api'
import { Card, EmptyState, SkeletonRows } from '../../ui/controls'
import { FileIcon } from '../../ui/FileIcon'
import { formatUntil, formatWhen } from '../../ui/when'
import { useShares } from './queries'
import { LINK_NOT_KEPT, LinkField } from './ShareDialog'
import { useShareUrl } from './shareUrls'
import { useShareCommands, type ShareCommands } from './useShareCommands'

/**
 * `/shared` — every link this account has out, newest first.
 *
 * One row per active share: the file (a link into its folder with the viewer
 * open), when the link was made, when it stops, how many downloads against
 * what cap, and whether a password or the trash stands in front of it. The
 * actions are the dialog's — Settings, New link, Stop sharing — through the
 * same `useShareCommands`, and Copy appears only on a row whose URL this
 * browser minted, because the server cannot hand it out again; any other row
 * says so.
 *
 * Paged like a folder and the trash, with the same Load more: a single-page
 * list that quietly showed "some" would be the second such finding, not a
 * trade.
 */
export function SharedLinksPage() {
  const shares = useShares()
  const commands = useShareCommands()

  const items = shares.data?.pages.flatMap((page) => page.items) ?? []

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-col gap-3 px-4 py-5 sm:px-6 sm:py-6">
      <div>
        <h1 className="text-[17px] font-semibold tracking-tight">Shared links</h1>
        <p className="text-[13px] text-ink-3">
          Stopping a link stops new downloads; a download already running finishes.
        </p>
      </div>

      <Card>
        {shares.isPending && <SkeletonRows />}

        {shares.error != null && items.length === 0 && (
          <div role="alert" className="flex flex-col items-start gap-2 px-4 py-6">
            <p className="text-sm text-ink-2">The shared links didn’t load.</p>
            <Button variant="outline" size="sm" onClick={() => void shares.refetch()}>
              Try again
            </Button>
          </div>
        )}

        {shares.isSuccess && items.length === 0 && (
          <EmptyState
            icon={<Link2 className="size-5" />}
            title="No share links yet."
            hint="Share a file from its row menu and the link turns up here."
          />
        )}

        {items.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="h-9 border-b border-line bg-surface-muted text-left text-[12px] font-medium text-ink-3">
                  <th scope="col" className="px-3 font-medium sm:px-4">
                    File
                  </th>
                  <th scope="col" className="hidden px-3 font-medium md:table-cell">
                    Created
                  </th>
                  <th scope="col" className="px-3 font-medium">
                    Expires
                  </th>
                  <th scope="col" className="px-3 font-medium">
                    Downloads
                  </th>
                  <th scope="col" className="px-3 sm:px-4">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {items.map((share) => (
                  <ShareRow key={share.id} share={share} commands={commands} />
                ))}
              </tbody>
            </table>
          </div>
        )}

        {shares.hasNextPage && (
          <div className="border-t border-line px-4 py-3">
            <Button
              variant="outline"
              size="sm"
              onClick={() => void shares.fetchNextPage()}
              disabled={shares.isFetchingNextPage}
            >
              {shares.isFetchingNextPage ? 'Loading…' : 'Load more'}
            </Button>
          </div>
        )}
      </Card>

      {commands.dialogs}
    </main>
  )
}

function ShareRow({ share, commands }: { share: Share; commands: ShareCommands }) {
  const url = useShareUrl(share.id)
  const { node } = share
  // The file, with the viewer open on it. A file always has a parent; the
  // root stands in for the one case the type allows and the server never sends.
  const to = `${node.parent_id === null ? '/' : `/folders/${node.parent_id}`}?preview=${encodeURIComponent(node.id)}`

  return (
    <tr data-testid="share-row" className="align-top">
      <td className="px-3 py-3 sm:px-4">
        <div className="flex items-center gap-2">
          <FileIcon kind="file" name={node.name} mime={node.mime} size={20} />
          <Link to={to} className="min-w-0 truncate text-ink hover:underline">
            {node.name}
          </Link>
          {share.has_password && <Badge>Password</Badge>}
          {!share.node_live && <Badge tone="warn">In trash</Badge>}
        </div>
        {url !== undefined ? (
          <div className="mt-2 max-w-md">
            <LinkField key={url} url={url} />
          </div>
        ) : (
          <p className="mt-1 text-[13px] text-ink-3">{LINK_NOT_KEPT}</p>
        )}
      </td>
      <td className="hidden px-3 py-3 text-[13px] text-ink-3 md:table-cell">{formatWhen(share.created_at)}</td>
      <td className="px-3 py-3 text-[13px] text-ink-3">{formatUntil(share.expires_at)}</td>
      <td className="numeric px-3 py-3 text-ink-3">
        {share.max_downloads === null ? share.download_count : `${share.download_count} of ${share.max_downloads}`}
      </td>
      <td className="px-3 py-2 sm:px-4">
        <div className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => commands.settings(share)}>
            Settings
          </Button>
          <Button variant="ghost" size="sm" disabled={commands.busy} onClick={() => commands.newLink(share)}>
            New link
          </Button>
          <Button
            variant="ghost"
            size="sm"
            className="hover:bg-danger-soft hover:text-danger"
            disabled={commands.busy}
            onClick={() => commands.stopSharing(share)}
          >
            Stop sharing
          </Button>
        </div>
      </td>
    </tr>
  )
}

function Badge({ children, tone = 'plain' }: { children: string; tone?: 'plain' | 'warn' }) {
  return (
    <span
      className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium ${
        tone === 'warn' ? 'bg-warn-soft text-warn' : 'bg-surface-muted text-ink-2'
      }`}
    >
      {children}
    </span>
  )
}
