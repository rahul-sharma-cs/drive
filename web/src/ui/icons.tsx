/**
 * The glyph set — hand-drawn on a 16px grid and stroked in `currentColor`
 * (`DriveMark` is the one filled exception), all decorative. Every icon sits
 * next to a text label, so each one is `aria-hidden`: the accessible name comes
 * from the words, never from the drawing.
 *
 * Inline rather than a library: a handful of glyphs does not justify a
 * dependency, and the SPA ships inside the server binary — every byte here is
 * a byte the browser does not fetch.
 */

type IconProps = { className?: string }

function Svg({ className, children }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={className ?? 'h-4 w-4'}
    >
      {children}
    </svg>
  )
}

/** The mark: three stacked bands, the way a file is actually stored here. */
export function DriveMark({ className }: IconProps) {
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true" className={className ?? 'h-5 w-5'}>
      <rect x="1.5" y="2" width="13" height="3.4" rx="1.2" fill="currentColor" opacity="0.35" />
      <rect x="1.5" y="6.3" width="13" height="3.4" rx="1.2" fill="currentColor" opacity="0.65" />
      <rect x="1.5" y="10.6" width="13" height="3.4" rx="1.2" fill="currentColor" />
    </svg>
  )
}

export function FolderIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path d="M1.75 4.25c0-.55.45-1 1-1h3.09c.3 0 .58.13.77.36l.78.93h5.86c.55 0 1 .45 1 1v6.2c0 .55-.45 1-1 1H2.75c-.55 0-1-.45-1-1z" />
    </Svg>
  )
}




export function UploadIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path d="M8 10.6V2.6m0 0L5.3 5.3M8 2.6l2.7 2.7" />
      <path d="M2.6 10v2.4c0 .55.45 1 1 1h8.8c.55 0 1-.45 1-1V10" />
    </Svg>
  )
}


/** Used only where an upload needs a person: conflicts, expiry, a lost file. */
export function AlertIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <circle cx="8" cy="8" r="6.2" />
      <path d="M8 4.9v3.6M8 11.1h.01" />
    </Svg>
  )
}

export function ChevronIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path d="m4.5 6.3 3.5 3.4 3.5-3.4" />
    </Svg>
  )
}


