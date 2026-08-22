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
 * SVG is refused here as well as there. It is an image element away from being
 * script running on the store's origin, and the redundancy costs one line.
 */

export type PreviewKind = 'image' | 'video' | 'audio' | 'text' | 'pdf' | 'none'

/**
 * The fallback for a link that arrived without a type at all. Deliberately
 * short, and deliberately without `svg` or `html`: a name is not evidence.
 */
const BY_EXTENSION: Record<string, PreviewKind> = {
  png: 'image',
  jpg: 'image',
  jpeg: 'image',
  gif: 'image',
  webp: 'image',
  avif: 'image',
  mp4: 'video',
  webm: 'video',
  mp3: 'audio',
  ogg: 'audio',
  wav: 'audio',
  pdf: 'pdf',
  txt: 'text',
  md: 'text',
  csv: 'text',
  json: 'text',
  log: 'text',
}

export function previewKind(mime: string | null | undefined, name: string): PreviewKind {
  const type = (mime ?? '').split(';')[0].trim().toLowerCase()

  if (type !== '') {
    if (type === 'image/svg+xml') return 'none'
    if (type === 'application/pdf') return 'pdf'
    if (type === 'text/plain') return 'text'
    if (type.startsWith('image/')) return 'image'
    if (type.startsWith('video/')) return 'video'
    if (type.startsWith('audio/')) return 'audio'
    // A type the server named and this viewer has no body for. The name cannot
    // overrule it — a `.png` served as `application/zip` is a zip.
    return 'none'
  }

  const ext = name.toLowerCase().split('.').pop()
  return (ext && BY_EXTENSION[ext]) || 'none'
}
