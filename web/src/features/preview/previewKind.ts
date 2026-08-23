/**
 * Which body the viewer renders for a type.
 *
 * The argument is the server's own normalized content type — the one it signed
 * the link with — and not the stored, client-declared `mime` on the row. The
 * server decides what may be served inline at all; this only decides which
 * element shows it. Passing the row's own mime here would put that decision
 * back in the client's hands, which is the whole thing the allowlist exists to
 * prevent.
 *
 * A file's *name* is not a signal either, and there is deliberately no fallback
 * to its extension. The kind such a fallback reaches that matters is `pdf`, and
 * the PDF body is the one that is a navigable cross-origin frame: guessing it
 * from the end of a name would put bytes the server declined to describe into a
 * frame the browser navigates to. A link that arrives without a type is a link
 * this viewer has nothing to say about.
 *
 * SVG is refused here as well as there. It is an image element away from being
 * script running on the store's origin, and the redundancy costs one line.
 */

export type PreviewKind = 'image' | 'video' | 'audio' | 'text' | 'pdf' | 'none'

export function previewKind(mime: string | null | undefined): PreviewKind {
  const type = (mime ?? '').split(';')[0].trim().toLowerCase()

  if (type === 'image/svg+xml') return 'none'
  if (type === 'application/pdf') return 'pdf'
  if (type === 'text/plain') return 'text'
  if (type.startsWith('image/')) return 'image'
  if (type.startsWith('video/')) return 'video'
  if (type.startsWith('audio/')) return 'audio'
  // A type the server named and this viewer has no body for, or no type at all.
  // The name cannot overrule either: a `.png` served as `application/zip` is a
  // zip, and a `.pdf` served as nothing is nothing.
  return 'none'
}
