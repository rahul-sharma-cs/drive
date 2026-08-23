/**
 * The wordmark, and nothing else.
 *
 * This file used to carry a small hand-drawn glyph set. Everything in it that
 * was a *generic* symbol — a folder, an upload arrow, a warning circle, a
 * chevron — is now the lucide glyph of the same name, so the product draws one
 * icon set instead of two side by side. What is left is the one drawing that
 * could not come from a library because it is Drive's own: three stacked
 * bands, the way a file is actually stored here.
 *
 * Decorative, like every icon in the app: it always sits beside the word
 * "Drive", so the accessible name comes from the text.
 */

/** The mark: three stacked bands, the way a file is actually stored here. */
export function DriveMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true" className={className ?? 'h-5 w-5'}>
      <rect x="1.5" y="2" width="13" height="3.4" rx="1.2" fill="currentColor" opacity="0.35" />
      <rect x="1.5" y="6.3" width="13" height="3.4" rx="1.2" fill="currentColor" opacity="0.65" />
      <rect x="1.5" y="10.6" width="13" height="3.4" rx="1.2" fill="currentColor" />
    </svg>
  )
}
