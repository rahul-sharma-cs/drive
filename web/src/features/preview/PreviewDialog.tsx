import { ChevronLeft, ChevronRight, Download, X } from 'lucide-react'
import { useRef } from 'react'

import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'

import { downloadHref, type DriveNode, type PreviewLink } from '../../lib/api'
import { FileIcon } from '../../ui/FileIcon'
import { usePreview, usePreviewParam, type Preview } from './usePreview'

/**
 * The viewer.
 *
 * Rendered by the screen that owns the list — a folder, a search — rather than
 * by the list itself, so opening a file never remounts the rows underneath it.
 * Which file is open comes from the URL, and the siblings the arrows walk are
 * the rows that screen has actually loaded, in the order they are shown.
 *
 * Every byte on screen here comes straight from the object store over a
 * presigned URL: an element's `src`, or a bare `fetch` for text. Nothing is
 * fetched through `lib/api`'s `request()` (its `X-Drive-Client` header would
 * make the request a preflight the store refuses) and nothing is ever turned
 * into a `blob:` URL, which would inherit this app's origin — the one thing
 * serving other people's files inline must never do.
 */

export interface PreviewDialogProps {
  /** Every loaded row, in list order. Folders are skipped: they never preview. */
  nodes: readonly DriveNode[]
  /** The listing has pages it has not fetched, so the counter says "of N loaded". */
  hasMore?: boolean
}

export function PreviewDialog({ nodes, hasMore = false }: PreviewDialogProps) {
  const { id, show, close } = usePreviewParam()
  const preview = usePreview(id)

  // The dialog has to survive its own closing animation, so what it renders is
  // the last file it was asked for, not the parameter — which is already gone
  // by then. Same for the link: without this the picture would blink out and
  // an empty frame would fade away in its place.
  const lastId = useRef<string | null>(null)
  if (id !== null) lastId.current = id
  const shown = lastId.current
  const lastLink = useRef<PreviewLink | undefined>(undefined)
  if (preview.link) lastLink.current = preview.link
  const link = preview.link ?? (id === null ? lastLink.current : undefined)

  const files = nodes.filter((node) => node.kind === 'file')
  const index = shown === null ? -1 : files.findIndex((node) => node.id === shown)
  const node = index < 0 ? undefined : files[index]
  const previous = index > 0 ? files[index - 1] : undefined
  const next = index >= 0 && index < files.length - 1 ? files[index + 1] : undefined

  if (shown === null) return null

  const name = node?.name ?? 'Preview'

  return (
    <Dialog
      open={id !== null}
      onOpenChange={(open) => {
        if (!open) close()
      }}
    >
      <DialogContent
        showCloseButton={false}
        // Radix asks for a description or an explicit opt-out; the file's own
        // name is the whole of what there is to say about it.
        aria-describedby={undefined}
        onCloseAutoFocus={(event) => {
          // Radix returns focus to the trigger, and this dialog has none — it
          // was opened by following a link. Put it back on that link, which is
          // where the person left it.
          const row = document.querySelector<HTMLElement>(`[data-preview-id="${shown}"]`)
          if (!row) return
          event.preventDefault()
          row.focus()
        }}
        onKeyDown={(event) => {
          if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
          // Inside a media control or a text field the arrows already mean
          // something — seeking, moving a caret.
          if ((event.target as HTMLElement).closest?.('video, audio, input, textarea')) return
          const to = event.key === 'ArrowLeft' ? previous : next
          if (!to) return
          event.preventDefault()
          show(to.id)
        }}
        // 0.98 rather than the primitive's 0.95: a surface this large reads as
        // a lurch at the smaller scale. The duration and the scrim's fade come
        // from the primitive, and the stylesheet's reduced-motion block turns
        // both off.
        className="flex h-[86vh] max-h-[900px] w-full max-w-[min(96vw,1400px)] flex-col gap-0 overflow-hidden rounded-card border-line p-0 duration-200 data-[state=closed]:zoom-out-98 data-[state=open]:zoom-in-98 sm:max-w-[min(96vw,1400px)]"
      >
        <DialogHeader className="flex-row items-center gap-3 border-b border-line px-3 py-2.5 text-left sm:px-4">
          <FileIcon kind="file" name={node?.name ?? ''} mime={node?.mime ?? link?.mime} size={24} />
          <DialogTitle className="min-w-0 flex-1 truncate text-[15px] leading-normal font-medium">
            {name}
          </DialogTitle>
          {index >= 0 && (
            <span className="numeric shrink-0 text-[12px] text-ink-3">
              {index + 1} of {files.length}
              {hasMore ? ' loaded' : ''}
            </span>
          )}
          <Button asChild variant="outline" size="sm">
            {/* The bytes come from the object store; this app only ever points
                at them, and in a tab of their own so a 401 cannot replace the
                app with a JSON body. */}
            <a
              href={downloadHref(shown)}
              target="_blank"
              rel="noopener"
              draggable={false}
              // Narrow screens keep the icon and drop the word; the name stays.
              aria-label="Download"
            >
              <Download />
              <span className="hidden sm:inline">Download</span>
            </a>
          </Button>
          <DialogClose asChild>
            <Button variant="ghost" size="icon-sm" aria-label="Close">
              <X />
            </Button>
          </DialogClose>
        </DialogHeader>

        <div className="relative flex min-h-0 flex-1 items-center justify-center overflow-hidden bg-canvas p-3 sm:p-4">
          <PreviewBody id={shown} node={node} preview={preview} link={link} />

          {(previous || next) && (
            // Below `sm` there is no room beside the picture, so the pair sits
            // at the bottom; from `sm` up they take an edge each.
            <div className="pointer-events-none absolute inset-0 flex items-end justify-center gap-3 p-3 sm:items-center sm:justify-between sm:p-2">
              <StepButton label="Previous file" icon={ChevronLeft} to={previous} onStep={show} />
              <StepButton label="Next file" icon={ChevronRight} to={next} onStep={show} />
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}

function StepButton({
  label,
  icon: Icon,
  to,
  onStep,
}: {
  label: string
  icon: typeof ChevronLeft
  to: DriveNode | undefined
  onStep: (id: string) => void
}) {
  return (
    <Button
      variant="outline"
      size="icon"
      aria-label={label}
      // Kept in place rather than dropped when there is nowhere to go, so the
      // other button does not slide across the picture at the ends of a list.
      disabled={to === undefined}
      className="pointer-events-auto rounded-full shadow-pop disabled:opacity-30"
      onClick={() => to && onStep(to.id)}
    >
      <Icon />
    </Button>
  )
}

function PreviewBody({
  id,
  node,
  preview,
  link,
}: {
  id: string
  node: DriveNode | undefined
  preview: Preview
  link: PreviewLink | undefined
}) {
  const card = <NoPreviewCard id={id} node={node} />

  if (preview.pending && link === undefined) {
    return <p className="text-sm text-ink-3">Loading the preview…</p>
  }
  if (link === undefined || preview.broken) return card

  // Pictures today. Every other type the server will serve inline gets its own
  // element next, and everything else keeps the card.
  if (!link.mime.startsWith('image/')) return card
  return (
    <img
      src={link.url}
      alt={node?.name ?? ''}
      onError={preview.onBroken}
      className="max-h-full max-w-full object-contain"
    />
  )
}

/** The answer for a type this app will not show, and for one it could not. */
function NoPreviewCard({ id, node }: { id: string; node: DriveNode | undefined }) {
  return (
    <div
      data-testid="no-preview"
      className="flex flex-col items-center gap-3 rounded-card border border-line bg-surface px-8 py-10 text-center"
    >
      <FileIcon kind="file" name={node?.name ?? ''} mime={node?.mime} size={44} />
      <p className="text-sm font-medium text-ink">No preview for this type</p>
      <p className="max-w-xs text-[13px] text-ink-3">
        Download it to open it in whatever handles it best.
      </p>
      <Button asChild variant="outline" size="sm">
        <a href={downloadHref(id)} target="_blank" rel="noopener" draggable={false}>
          <Download />
          Download
        </a>
      </Button>
    </div>
  )
}
