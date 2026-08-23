import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

import { downloadHref } from '../../lib/api'
import { FileIcon } from '../../ui/FileIcon'
import { formatBytes } from '../../ui/format'
import { dismissZipOffer, useZipOffer } from './useZipDownload'

/**
 * What is offered when the archive cannot be built here: the files themselves,
 * one link each.
 *
 * This is the browser without File System Access — Safari, Firefox — past the
 * point where the whole archive would have to be held in memory before it could
 * be handed over. The app cannot make that download happen; only a person can,
 * because each file costs one click's worth of the browser's permission to
 * navigate. So the honest thing is a list of links and no promises: every row
 * is a plain `<a download>` at `/api/files/{id}/download`, which answers a 302
 * to a short-lived attachment URL — a same-tab navigation that downloads
 * without the page going anywhere, so the list stays open for the next one.
 *
 * The archive's folders are gone here, so each row shows the full path it would
 * have had rather than just its name: it is the only thing telling two
 * `report.pdf`s apart.
 */
export function IndividualDownloads() {
  const offer = useZipOffer()
  if (offer === null) return null

  return (
    <Dialog open onOpenChange={(open) => !open && dismissZipOffer()}>
      <DialogContent showCloseButton={false} className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Too large to zip in this browser</DialogTitle>
          <DialogDescription>
            This selection is {formatBytes(offer.total)} — too large to pack in this browser. Download the files one at
            a time; the folders they sit in are not kept.
          </DialogDescription>
        </DialogHeader>

        <ul className="-mx-2 max-h-[50vh] overflow-y-auto">
          {offer.files.map((entry) => (
            <li key={entry.id}>
              <a
                href={downloadHref(entry.id)}
                download
                title={entry.path}
                className="flex items-center gap-2.5 rounded-control px-2 py-1.5 text-sm text-ink
                           transition duration-100 hover:bg-surface-muted"
              >
                <FileIcon kind="file" name={baseName(entry.path)} />
                <span className="min-w-0 flex-1 truncate">{entry.path}</span>
                <span className="numeric shrink-0 text-ink-3">{formatBytes(entry.size)}</span>
              </a>
            </li>
          ))}
        </ul>

        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  )
}

const baseName = (path: string): string => path.slice(path.lastIndexOf('/') + 1)
