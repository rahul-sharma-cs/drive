/**
 * What kind of thing a row holds, in one glyph: an amber filled folder, or a
 * page carrying the mark of its type in that type's colour. Rows, the viewer
 * title and the destination picker all render off the one `FILE_ICON_TABLE`
 * below, so a new file type or a future retint is one row changed in one
 * place, not a resolver edited in four.
 *
 * Every file glyph is page-shaped and every folder is not, so the silhouette
 * answers "is this somewhere I can go into?" before any colour is read. The
 * mark inside the page and its hue answer "what is it?" — which is how Drive
 * and OneDrive both do it, and it is the only thing that stays legible at the
 * 22px a dense list can spend.
 *
 * A file also carries its extension as a tag — `PDF`, `JPEG` — in the same
 * hue, the second half of the original decision (per-type glyphs *with* an
 * extension tag), which the design pass had dropped. The tag sits beside the
 * glyph, not inside it: three letters over the ruled lines of a 22px page
 * were tried and smudged, so it is a tiny mono label to the right, inside the
 * icon's own inline-flex so it can never wrap the name. Folders and files of
 * no known type get none, and neither does a name with no extension.
 *
 * Purely decorative: a row renders the file's name as text next to this, so
 * neither the glyph nor the tag carries information a screen reader needs.
 */

import type { LucideIcon } from 'lucide-react'
import {
  File,
  FileArchive,
  FileAudio,
  FileCode,
  FileImage,
  FileSpreadsheet,
  FileText,
  FileType,
  FileVideo,
  Folder,
  Presentation,
} from 'lucide-react'

export type FileCategory =
  | 'folder'
  | 'pdf'
  | 'image'
  | 'video'
  | 'audio'
  | 'archive'
  | 'spreadsheet'
  | 'document'
  | 'presentation'
  | 'code'
  | 'text'
  | 'generic'

interface CategorySpec {
  category: FileCategory
  icon: LucideIcon
  /**
   * A text-colour utility off one of the product's own tokens — never a stock
   * palette shade. The type hues are declared as a closed set in `index.css`
   * beside the three UI hues, which is what keeps a file list reading as one
   * palette rather than as Drive's next to Tailwind's.
   */
  colorClass: string
  /** Solid rather than outline — folders only, so far. */
  filled?: boolean
  extensions: string[]
  mimeExact?: string[]
  /** Checked with `startsWith`, e.g. `'image/'`. */
  mimePrefixes?: string[]
}

/**
 * One row per file category — the single table every type colour in the
 * product comes out of. Deliberately excludes `folder` and `generic`, which have no extensions
 * to classify by and live in `FOLDER_SPEC`/`GENERIC_SPEC` below; keeping them
 * out of this array is what makes the "no duplicate extension" test meaningful
 * instead of vacuous.
 */
export const FILE_ICON_TABLE: CategorySpec[] = [
  {
    category: 'pdf',
    icon: FileText,
    colorClass: 'text-danger',
    extensions: ['pdf'],
    mimeExact: ['application/pdf'],
  },
  {
    category: 'image',
    icon: FileImage,
    colorClass: 'text-type-image',
    extensions: ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'heic'],
    mimePrefixes: ['image/'],
  },
  {
    category: 'video',
    icon: FileVideo,
    colorClass: 'text-type-video',
    extensions: ['mp4', 'webm', 'mov', 'mkv'],
    mimePrefixes: ['video/'],
  },
  {
    category: 'audio',
    icon: FileAudio,
    colorClass: 'text-type-audio',
    extensions: ['mp3', 'ogg', 'wav', 'm4a', 'flac'],
    mimePrefixes: ['audio/'],
  },
  {
    category: 'archive',
    icon: FileArchive,
    colorClass: 'text-type-archive',
    extensions: ['zip', 'tar', 'gz', '7z', 'rar', 'tar.gz'],
    mimeExact: [
      'application/zip',
      'application/x-zip-compressed',
      'application/gzip',
      'application/x-gzip',
      'application/x-tar',
      'application/x-7z-compressed',
      'application/vnd.rar',
      'application/x-rar-compressed',
    ],
  },
  {
    category: 'spreadsheet',
    icon: FileSpreadsheet,
    colorClass: 'text-type-sheet',
    extensions: ['csv', 'xlsx', 'xls', 'ods'],
    mimeExact: [
      'text/csv',
      'application/vnd.ms-excel',
      'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
      'application/vnd.oasis.opendocument.spreadsheet',
    ],
  },
  {
    // A letterform inside the page, not the ruled lines `FileText` draws. PDFs,
    // Word documents and plain text all had the same drawing, which left the
    // hue carrying the whole difference between three of the commonest things
    // in a folder — and two of those hues are a blue and a grey that a small
    // glyph does not hold apart at 20px. The page silhouette is intact, the way
    // every file glyph's is; what changed is the mark inside it.
    category: 'document',
    icon: FileType,
    colorClass: 'text-type-doc',
    extensions: ['docx', 'doc', 'odt', 'rtf'],
    mimeExact: [
      'application/msword',
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
      'application/vnd.oasis.opendocument.text',
      'application/rtf',
      'text/rtf',
    ],
  },
  {
    category: 'presentation',
    icon: Presentation,
    colorClass: 'text-type-doc',
    extensions: ['pptx', 'key'],
    mimeExact: [
      'application/vnd.ms-powerpoint',
      'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    ],
  },
  {
    category: 'code',
    icon: FileCode,
    colorClass: 'text-type-code',
    extensions: ['js', 'ts', 'tsx', 'go', 'py', 'rs', 'html', 'css', 'json', 'yaml', 'yml', 'sh'],
  },
  {
    category: 'text',
    icon: FileText,
    colorClass: 'text-type-text',
    extensions: ['txt', 'md'],
    mimeExact: ['text/plain', 'text/markdown'],
  },
]

const FOLDER_SPEC: CategorySpec = {
  category: 'folder',
  icon: Folder,
  colorClass: 'text-type-folder',
  filled: true,
  extensions: [],
}

const GENERIC_SPEC: CategorySpec = {
  category: 'generic',
  icon: File,
  colorClass: 'text-ink-3',
  extensions: [],
}

const EXTENSION_TO_SPEC = new Map<string, CategorySpec>()
const MIME_EXACT_TO_SPEC = new Map<string, CategorySpec>()
const MIME_PREFIX_TO_SPEC: Array<[string, CategorySpec]> = []

for (const spec of FILE_ICON_TABLE) {
  for (const ext of spec.extensions) EXTENSION_TO_SPEC.set(ext, spec)
  for (const mime of spec.mimeExact ?? []) MIME_EXACT_TO_SPEC.set(mime, spec)
  for (const prefix of spec.mimePrefixes ?? []) MIME_PREFIX_TO_SPEC.push([prefix, spec])
}

function normalizeMime(mime?: string | null): string | null {
  if (!mime) return null
  const stripped = mime.split(';')[0]?.trim().toLowerCase()
  return stripped || null
}

/**
 * The extension used for lookup, compound-aware: `archive.tar.gz` checks
 * `tar.gz` before falling back to `gz`. Dotfiles (`.env`) and extension-less
 * names count as having no extension, not an extension equal to the whole
 * name.
 */
function extractExtension(name: string): string | null {
  const lower = name.toLowerCase()
  const dot = lower.lastIndexOf('.')
  if (dot <= 0 || dot === lower.length - 1) return null
  const parts = lower.split('.')
  if (parts.length >= 3) {
    const compound = parts.slice(-2).join('.')
    if (EXTENSION_TO_SPEC.has(compound)) return compound
  }
  return lower.slice(dot + 1)
}

/** Resolution order: folder beats everything; mime beats the extension; the extension beats generic. */
function classify(kind: 'file' | 'folder', name: string, mime?: string | null): CategorySpec {
  if (kind === 'folder') return FOLDER_SPEC

  const normalizedMime = normalizeMime(mime)
  const ext = extractExtension(name)

  if (normalizedMime) {
    const exact = MIME_EXACT_TO_SPEC.get(normalizedMime)
    if (exact) return exact
    const prefixed = MIME_PREFIX_TO_SPEC.find(([prefix]) => normalizedMime.startsWith(prefix))
    if (prefixed) return prefixed[1]
  }

  if (ext) {
    const bySpec = EXTENSION_TO_SPEC.get(ext)
    if (bySpec) return bySpec
  }

  return GENERIC_SPEC
}

/**
 * Classification only, for callers that never render — the upload dock's
 * grouping, the destination picker's folder-only filter.
 */
export function fileCategory(kind: 'file' | 'folder', name: string, mime?: string | null): FileCategory {
  return classify(kind, name, mime).category
}

export interface FileIconProps {
  kind: 'file' | 'folder'
  name: string
  mime?: string | null
  /** Pixels. Rows want 20-24; the viewer title passes a larger value. */
  size?: number
  className?: string
}

/**
 * The tag beside a file glyph: the last extension in upper case, when it is
 * two to four plain characters — `PDF`, `JPEG`, the `GZ` of `.tar.gz`. A
 * longer or odder extension is not a type anyone reads at a glance, and the
 * name beside it spells it out anyway.
 */
export function extensionTag(name: string): string | null {
  const ext = extractExtension(name)?.split('.').pop()
  return ext !== undefined && /^[a-z0-9]{2,4}$/.test(ext) ? ext.toUpperCase() : null
}

export function FileIcon({ kind, name, mime, size = 20, className = '' }: FileIconProps) {
  const spec = classify(kind, name, mime)
  const Icon = spec.icon
  const tag = spec === FOLDER_SPEC || spec === GENERIC_SPEC ? null : extensionTag(name)

  return (
    <span className={`inline-flex shrink-0 items-center gap-1 ${className}`}>
      <Icon
        aria-hidden="true"
        size={size}
        className={`shrink-0 ${spec.colorClass}`}
        {...(spec.filled ? { fill: 'currentColor' } : {})}
      />
      {tag !== null && (
        <span aria-hidden="true" className={`font-mono text-[10px] leading-none tracking-wide ${spec.colorClass}`}>
          {tag}
        </span>
      )}
    </span>
  )
}
