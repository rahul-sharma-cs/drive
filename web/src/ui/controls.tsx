/**
 * The shared controls. Three button weights and one input, because a file
 * manager only ever asks for three levels of emphasis: the one action a screen
 * is for, the actions beside it, and the actions on a row.
 *
 * Every control answers on press rather than on release — the scale step is on
 * `:active`, not on `click` — and the focus ring is the single global one from
 * `index.css`, so a keyboard user sees the same ring everywhere.
 */

import type { ReactNode } from 'react'

import { ApiError } from '../lib/api'
import { AlertIcon, DriveMark } from './icons'

const pressable =
  'inline-flex items-center justify-center gap-1.5 rounded-control font-medium transition duration-100 ease-out ' +
  'active:scale-[0.98] disabled:pointer-events-none disabled:opacity-45'

/** The one action a screen exists for. */
export const buttonClass = `${pressable} bg-accent px-4 py-2 text-sm text-white hover:bg-accent-strong`

/** Everything alongside it: toolbars, dialog cancels, row actions. */
export const secondaryButtonClass =
  `${pressable} border border-line-strong bg-surface px-2.5 py-1.5 text-[13px] text-ink-2 ` +
  'shadow-card hover:border-ink-3 hover:text-ink'

/** Quiet by default, present on hover — for repeated actions on every row. */
export const ghostButtonClass =
  `${pressable} px-2 py-1.5 text-[13px] text-ink-3 hover:bg-surface-muted hover:text-ink`

/** The same, for the action that throws something away. */
export const dangerButtonClass =
  `${pressable} px-2 py-1.5 text-[13px] text-ink-3 hover:bg-danger-soft hover:text-danger`

export const inputClass =
  'w-full rounded-control border border-line-strong bg-surface px-3 py-2 text-sm text-ink outline-none ' +
  'transition duration-100 placeholder:text-ink-3 focus:border-accent focus:ring-2 focus:ring-accent/20'

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
      <AlertIcon className="mt-0.5 h-4 w-4 shrink-0" />
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

/** The shape of the answer while it is still being fetched. */
export function SkeletonRows({ rows = 3 }: { rows?: number }) {
  return (
    <ul aria-hidden="true" className="divide-y divide-line">
      {Array.from({ length: rows }, (_, i) => (
        <li key={i} className="flex items-center gap-3 px-4 py-3">
          <span className="skeleton h-4 w-4 rounded" />
          <span className="skeleton h-3.5 rounded" style={{ width: `${38 - i * 7}%` }} />
          <span className="skeleton ml-auto h-3 w-12 rounded" />
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
        <p className="numeric text-accent-soft/70 uppercase">Self-hosted file storage</p>
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
                  i < confirmed ? 'bg-accent part-lit' : 'bg-canvas/15'
                }`}
                style={{ animationDelay: `${120 + i * 70}ms` }}
              />
            ))}
          </div>
          <p className="numeric mt-2 text-canvas/50">
            A resumed upload: the lit parts are already stored · {total - confirmed} left to send
          </p>
        </div>
      </div>

      <p className="text-[13px] text-canvas/40">Go · React · S3-compatible storage · one binary</p>
    </aside>
  )
}
