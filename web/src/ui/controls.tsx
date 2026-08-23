/**
 * What is left of the shared controls once the buttons and the inputs became
 * `Button` and `Input`: the pieces that are Drive's own shapes rather than a
 * primitive wearing Drive's palette — a panel, a form error, an empty screen,
 * a loading row, and the signed-out frame.
 *
 * There used to be four button class strings and an input class string here
 * too, which meant the auth screens and the dialogs said "primary button" one
 * way while the file browser said it another. They are gone: every *button*
 * in the product — anything that reads as a button — is now `<Button
 * variant=…>`, pointed at the tokens below by the variable block in
 * `index.css`.
 *
 * Three controls are deliberately not primitives, and it is worth naming them
 * so the next sweep does not spend an afternoon rediscovering why:
 *
 *  - The **column headers** in `FileList` (`ColumnLabel`). They are `<button>`
 *    because they are clickable, but they are laid out as table headings —
 *    `flex-1`, a fixed 8rem column, `justify-end` — at the header row's own
 *    type scale. `Button` would impose a centred 36px pill with its own
 *    padding and a background on hover, and overriding all of that leaves
 *    nothing of the variant behind.
 *  - The **folder rows** in `DestinationDialog`. Same shape of answer: a
 *    full-width left-aligned list row, not a control sitting in a row.
 *  - `HeaderSearch`'s **pill field**, which is a shape of its own and says so
 *    where it is written.
 */

import { CircleAlert } from 'lucide-react'
import type { ReactNode } from 'react'

import { Skeleton } from '@/components/ui/skeleton'

import { ApiError } from '../lib/api'
import { DriveMark } from './icons'

/**
 * A labelled field: the caption above, the control below, wrapped so the two
 * are associated without an id to keep in sync.
 */
export const fieldClass = 'flex flex-col gap-1.5 text-[13px] font-medium text-ink-2'

/** A panel of content: the file list, the trash list, search results. */
export function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <section className={`overflow-hidden rounded-card border border-line bg-surface shadow-card ${className}`}>
      {children}
    </section>
  )
}

/**
 * Shows the server's own message. The API's error copy is written for people
 * ("verify your email first: check your inbox…"), so restating it here would
 * only make it vaguer; anything else is an unexpected failure and says so.
 */
export function FormError({ error }: { error: unknown }) {
  if (!error) return null
  const message =
    error instanceof ApiError ? error.message : 'Something went wrong. Check your connection and try again.'
  return (
    <p
      role="alert"
      className="flex items-start gap-2 rounded-control bg-danger-soft px-3 py-2 text-[13px] text-danger"
    >
      <CircleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
      <span>{message}</span>
    </p>
  )
}

/**
 * An empty screen is an invitation, not a dead end: it names what is missing
 * and what to do about it, in that order.
 */
export function EmptyState({ icon, title, hint }: { icon: ReactNode; title: string; hint?: string }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-14 text-center">
      <span className="flex h-10 w-10 items-center justify-center rounded-full bg-surface-muted text-ink-3">
        {icon}
      </span>
      <p className="text-sm font-medium text-ink-2">{title}</p>
      {hint && <p className="max-w-xs text-[13px] text-ink-3">{hint}</p>}
    </div>
  )
}

/**
 * The shape of the answer while it is still being fetched — a row's icon, its
 * name, its size, at the height a real row will land at, so nothing jumps when
 * the answer arrives.
 */
export function SkeletonRows({ rows = 3 }: { rows?: number }) {
  return (
    <ul aria-hidden="true" className="divide-y divide-line">
      {Array.from({ length: rows }, (_, i) => (
        <li key={i} className="flex h-12 items-center gap-3 px-3 sm:px-4">
          <Skeleton className="h-[22px] w-[22px] rounded-sm" />
          <Skeleton className="h-3.5" style={{ width: `${38 - i * 7}%` }} />
          <Skeleton className="ml-auto h-3 w-12" />
        </li>
      ))}
    </ul>
  )
}

/**
 * The signed-out frame, and the only place in the product that has to explain
 * itself: with signups gated, `/login` is the first screen anyone sees, and a
 * bare form on an empty page says nothing about what this is. The left panel
 * carries the claim; the right carries the form.
 */
export function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <main className="min-h-screen lg:grid lg:grid-cols-[1.05fr_1fr]">
      <ThesisPanel />
      <div className="mx-auto flex min-h-screen w-full max-w-sm flex-col justify-center gap-6 px-6 py-12">
        <div className="flex flex-col gap-3">
          <span className="flex h-9 w-9 items-center justify-center rounded-card bg-ink text-canvas lg:hidden">
            <DriveMark className="h-4 w-4" />
          </span>
          <h1 className="text-[1.75rem] leading-tight font-semibold">{title}</h1>
        </div>
        {children}
      </div>
    </main>
  )
}

/**
 * What the product is, said once, in its own vocabulary. The meter below the
 * copy is the same one the upload manager draws — twelve parts, nine of them
 * confirmed — because the claim and the interface should be the same object.
 */
function ThesisPanel() {
  const confirmed = 9
  const total = 12

  return (
    <aside className="relative hidden flex-col justify-between overflow-hidden bg-ink px-12 py-12 text-canvas lg:flex">
      <span className="flex items-center gap-2.5 text-[15px] font-semibold tracking-tight">
        <span className="flex h-7 w-7 items-center justify-center rounded-md bg-canvas text-ink">
          <DriveMark className="h-4 w-4" />
        </span>
        Drive
      </span>

      <div className="max-w-md">
        <p className="numeric text-teal-soft/70 uppercase">Self-hosted file storage</p>
        <h2 className="mt-3 text-[2.1rem] leading-[1.1] font-semibold tracking-[-0.02em]">
          Uploads that pick up
          <br />
          where they stopped.
        </h2>
        <p className="mt-4 text-[15px] leading-relaxed text-canvas/70">
          Files go from the browser straight to object storage, a part at a time. Every part the server confirms is a
          part you never send again — after a dropped connection, a closed tab, or a server that restarted underneath
          you.
        </p>

        <div className="mt-8">
          <div className="flex h-2 w-full max-w-sm gap-px" aria-hidden="true">
            {Array.from({ length: total }, (_, i) => (
              <span
                key={i}
                className={`h-full flex-1 first:rounded-l-full last:rounded-r-full ${
                  i < confirmed ? 'bg-teal part-lit' : 'bg-canvas/15'
                }`}
                style={{ animationDelay: `${120 + i * 70}ms` }}
              />
            ))}
          </div>
          <p className="numeric mt-2 text-canvas/50">A resumed upload — the lit parts are already stored</p>
        </div>
      </div>

      <p className="text-[13px] text-canvas/50">Go · React · S3-compatible storage · one binary</p>
    </aside>
  )
}
