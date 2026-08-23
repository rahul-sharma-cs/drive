import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, Download, X } from 'lucide-react'
import { useRef, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'

import { downloadHref, type DriveNode, type PreviewLink } from '../../lib/api'
import { FileIcon } from '../../ui/FileIcon'
import { previewKind } from './previewKind'
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

/**
 * Whether a framed PDF is worth showing at all.
 *
 * iOS Safari renders a PDF in an iframe as a blank box, and mobile Chrome
 * downloads it rather than displaying it — so on a touch device the frame is an
 * empty rectangle where an answer should be. A coarse primary pointer is the
 * signal (a laptop with a touchscreen still reports a fine one), and the honest
 * answer there is the download card.
 */
function framedPdfWorks(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return true
  return !window.matchMedia('(pointer: coarse)').matches
}

/**
 * Text is read into memory, so it gets a ceiling. Two megabytes is far past any
 * README and far short of a log file that would lock the tab up; above it the
 * honest answer is the download.
 */
const TEXT_LIMIT_BYTES = 2 * 1024 * 1024

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
          // Escaped: the id is a URL parameter, so it is whatever was typed
          // into the address bar, and an unescaped `"` in it turns this
          // selector into a syntax error that throws out of a focus handler.
          const row = document.querySelector<HTMLElement>(
            `[data-preview-id="${CSS.escape(shown)}"]`,
          )
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

  const kind = previewKind(link.mime)
  const name = node?.name ?? ''

  switch (kind) {
    case 'image':
      return (
        <img
          src={link.url}
          alt={name}
          onError={preview.onBroken}
          className="max-h-full max-w-full object-contain"
        />
      )
    case 'video':
      return (
        <video
          src={link.url}
          controls
          playsInline
          onError={preview.onBroken}
          className="max-h-full max-w-full"
        />
      )
    case 'audio':
      return <audio src={link.url} controls onError={preview.onBroken} className="w-full max-w-xl" />
    case 'pdf':
      if (!framedPdfWorks()) return card
      // A plain cross-origin frame, deliberately not a sandboxed one: measured
      // across three engines, Chromium runs its built-in viewer only in an
      // attribute-free iframe (`sandbox=""`, with or without `allow-scripts` /
      // `allow-same-origin`, gets the broken-document placeholder) and
      // Firefox's pdf.js paints its chrome but renders no pages, since its
      // viewer is itself script. The sandbox would buy a frame nobody can read.
      // What holds the line is elsewhere: the frame is the store's origin,
      // which none of this app's cookies reach, and the link is signed with a
      // forced `application/pdf`, so HTML wearing a `.pdf` name renders as
      // nothing at all.
      return (
        <iframe
          title={name || 'PDF preview'}
          src={link.url}
          referrerPolicy="no-referrer"
          className="h-full w-full rounded-card border border-line bg-surface"
        />
      )
    case 'text':
      return <TextBody url={link.url} card={card} />
    case 'none':
      return card
  }
}

/**
 * Text, read straight off the store.
 *
 * Keyed on the URL, so the minute-before-expiry refresh re-reads through the
 * new link rather than holding text fetched with a dead one.
 */
function TextBody({ url, card }: { url: string; card: ReactNode }) {
  const text = useQuery({
    queryKey: ['preview-text', url],
    queryFn: async ({ signal }) => {
      // Bare, deliberately: a custom header here would make this cross-origin
      // GET a preflight, and the store's rule answers preflights with a 403.
      const res = await fetch(url, { signal })
      if (!res.ok) throw new Error(`the file could not be read (${res.status})`)
      // A shortcut, not the guard: the header is absent on a chunked answer —
      // where `Number(null)` is 0, which is under every ceiling there is — and
      // on a gzipped one it describes the compressed length, which a 50 MB log
      // slips under. So the ceiling is enforced against the bytes themselves.
      const declared = Number(res.headers.get('Content-Length'))
      if (declared > TEXT_LIMIT_BYTES) return null
      return readCapped(res)
    },
    gcTime: 0,
  })

  if (text.isPending) return <p className="text-sm text-ink-3">Loading the preview…</p>
  // Too big to read, or unreadable: both end at the same honest offer.
  if (text.error || text.data == null) return card
  return (
    <pre className="numeric h-full w-full overflow-auto rounded-card border border-line bg-surface p-4 text-[12px] leading-relaxed text-ink">
      {text.data}
    </pre>
  )
}

/**
 * The body, up to the ceiling and not a byte past it.
 *
 * Read as it arrives rather than in one `res.text()`, because `text()` has no
 * ceiling: by the time it resolves the whole file is in memory, and the check
 * that follows can only decide whether to show what was already paid for. The
 * transfer is cancelled at the moment the count goes over, which is the only
 * thing that keeps a log file from being pulled down in full to be thrown away.
 */
async function readCapped(res: Response): Promise<string | null> {
  const reader = res.body?.getReader()
  // Nothing to stream — an answer with no body stream at all. The declared
  // length was the only guard there, and the whole of it is already here.
  if (!reader) {
    const body = await res.text()
    return new TextEncoder().encode(body).length > TEXT_LIMIT_BYTES ? null : body
  }

  const chunks: Uint8Array[] = []
  let read = 0
  for (;;) {
    const next = await reader.read()
    if (next.done) break
    read += next.value.length
    if (read > TEXT_LIMIT_BYTES) {
      await reader.cancel()
      return null
    }
    chunks.push(next.value)
  }

  // One decode over the joined bytes, not one per chunk: a character split
  // across a chunk boundary would otherwise come out as two replacements.
  const bytes = new Uint8Array(read)
  let at = 0
  for (const chunk of chunks) {
    bytes.set(chunk, at)
    at += chunk.length
  }
  return new TextDecoder().decode(bytes)
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

/**
 * Default as well as named: the named export is what the tests reach for, and
 * `React.lazy` in `PreviewDialog.lazy.tsx` needs the default.
 */
export default PreviewDialog
