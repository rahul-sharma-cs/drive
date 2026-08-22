/**
 * A coloured glyph plus a short extension tag — folders read amber and
 * filled, files read by type (a red "PDF" tag, a photo glyph tagged "PNG",
 * and so on) rather than as one undifferentiated file icon. Rows, the viewer
 * title, the upload dock, and the destination picker all render off the one
 * `FILE_ICON_TABLE` below, so a new file type or a future retint is one row
 * changed in one place, not a resolver edited in four.
 *
 * Purely decorative: a row already renders the file's name as text next to
 * this, so the glyph — and its tag — carry no information a screen reader
 * needs.
 */

import type { LucideIcon } from 'lucide-react'
import { File, FileArchive, FileAudio, FileCode, FileSpreadsheet, FileText, Folder, Image, Presentation, Video } from 'lucide-react'

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
  /** A Tailwind text-colour utility — the app's own tokens where one fits, a Tailwind default shade otherwise. */
  colorClass: string
  /** Solid rather than outline — folders only, so far. */
  filled?: boolean
  extensions: string[]
  mimeExact?: string[]
  /** Checked with `startsWith`, e.g. `'image/'`. Also the categories whose mime subtype doubles as a sensible tag (see `tagFor`). */
  mimePrefixes?: string[]
}

/**
 * One row per file category — the single table Phase F's design pass
 * retokenises from. Deliberately excludes `folder` and `generic`, which have
 * no extensions to classify by and live in `FOLDER_SPEC`/`GENERIC_SPEC`
 * below; keeping them out of this array is what makes the "no duplicate
 * extension" test meaningful instead of vacuous.
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
    icon: Image,
    colorClass: 'text-sky-600',
    extensions: ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'heic'],
    mimePrefixes: ['image/'],
  },
  {
    category: 'video',
    icon: Video,
    colorClass: 'text-violet-600',
    extensions: ['mp4', 'webm', 'mov', 'mkv'],
    mimePrefixes: ['video/'],
  },
  {
    category: 'audio',
    icon: FileAudio,
    colorClass: 'text-rose-600',
    extensions: ['mp3', 'ogg', 'wav', 'm4a', 'flac'],
    mimePrefixes: ['audio/'],
  },
  {
    category: 'archive',
    icon: FileArchive,
    colorClass: 'text-orange-600',
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
    colorClass: 'text-emerald-600',
    extensions: ['csv', 'xlsx', 'xls', 'ods'],
    mimeExact: [
      'text/csv',
      'application/vnd.ms-excel',
      'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
      'application/vnd.oasis.opendocument.spreadsheet',
    ],
  },
  {
    category: 'document',
    icon: FileText,
    colorClass: 'text-indigo-600',
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
    colorClass: 'text-orange-500',
    extensions: ['pptx', 'key'],
    mimeExact: [
      'application/vnd.ms-powerpoint',
      'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    ],
  },
  {
    category: 'code',
    icon: FileCode,
    colorClass: 'text-violet-500',
    extensions: ['js', 'ts', 'tsx', 'go', 'py', 'rs', 'html', 'css', 'json', 'yaml', 'yml', 'sh'],
  },
  {
    category: 'text',
    icon: FileText,
    colorClass: 'text-slate-500',
    extensions: ['txt', 'md'],
    mimeExact: ['text/plain', 'text/markdown'],
  },
]

const FOLDER_SPEC: CategorySpec = {
  category: 'folder',
  icon: Folder,
  colorClass: 'text-warn',
  filled: true,
  extensions: [],
}

const GENERIC_SPEC: CategorySpec = {
  category: 'generic',
  icon: File,
  colorClass: 'text-ink-3',
  extensions: [],
}

/** Extensions whose sensible tag isn't just their own uppercased text. */
const TAG_OVERRIDES: Record<string, string> = {
  jpeg: 'JPG',
  'tar.gz': 'TGZ',
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

interface Classification {
  spec: CategorySpec
  ext: string | null
  mime: string | null
}

/** Resolution order: folder beats everything; mime beats the extension; the extension beats generic. */
function classify(kind: 'file' | 'folder', name: string, mime?: string | null): Classification {
  if (kind === 'folder') return { spec: FOLDER_SPEC, ext: null, mime: null }

  const normalizedMime = normalizeMime(mime)
  const ext = extractExtension(name)

  if (normalizedMime) {
    const exact = MIME_EXACT_TO_SPEC.get(normalizedMime)
    if (exact) return { spec: exact, ext, mime: normalizedMime }
    const prefixed = MIME_PREFIX_TO_SPEC.find(([prefix]) => normalizedMime.startsWith(prefix))
    if (prefixed) return { spec: prefixed[1], ext, mime: normalizedMime }
  }

  if (ext) {
    const bySpec = EXTENSION_TO_SPEC.get(ext)
    if (bySpec) return { spec: bySpec, ext, mime: normalizedMime }
  }

  return { spec: GENERIC_SPEC, ext, mime: normalizedMime }
}

/**
 * Classification only, for callers that never render — the upload dock's
 * grouping, the destination picker's folder-only filter.
 */
export function fileCategory(kind: 'file' | 'folder', name: string, mime?: string | null): FileCategory {
  return classify(kind, name, mime).spec.category
}

function tagFor({ spec, ext, mime }: Classification): string | undefined {
  if (spec.category === 'folder') return undefined
  if (spec.category === 'pdf') return 'PDF'

  if (ext && EXTENSION_TO_SPEC.get(ext) === spec) return TAG_OVERRIDES[ext] ?? ext.toUpperCase()

  // No extension matched this spec (mime-only resolution, or no filename
  // extension at all) — image/video/audio mime subtypes double as sensible
  // tags ('image/png' -> 'PNG'); office mimes' subtypes don't, so this is
  // gated on `mimePrefixes` rather than applied to every category.
  if (mime && spec.mimePrefixes) {
    const subtype = mime.split('/')[1]?.split(/[+;]/)[0]
    if (subtype) return (TAG_OVERRIDES[subtype] ?? subtype).toUpperCase()
  }

  if (spec.category === 'generic' && ext && ext.length <= 3) return ext.toUpperCase()

  return undefined
}

export interface FileIconProps {
  kind: 'file' | 'folder'
  name: string
  mime?: string | null
  /** Pixels. Rows want 20-24; the viewer title and dock pass a larger value. */
  size?: number
  className?: string
}

export function FileIcon({ kind, name, mime, size = 20, className = '' }: FileIconProps) {
  const classification = classify(kind, name, mime)
  const { spec } = classification
  const tag = tagFor(classification)
  const Icon = spec.icon
  const tagSize = Math.max(6, Math.round(size * 0.32))

  return (
    <span
      aria-hidden="true"
      className={`relative inline-flex shrink-0 items-center justify-center ${className}`}
      style={{ width: size, height: size }}
    >
      <Icon aria-hidden="true" size={size} className={spec.colorClass} {...(spec.filled ? { fill: 'currentColor' } : {})} />
      {tag && (
        <span
          className={`absolute inset-x-0 bottom-[6%] text-center font-bold leading-none ${spec.colorClass}`}
          style={{ fontSize: tagSize }}
        >
          {tag}
        </span>
      )}
    </span>
  )
}
