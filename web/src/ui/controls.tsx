/**
 * The handful of shared control styles. Deliberately thin — the visual design
 * pass comes after behaviour is green, and anything richer than this would be
 * rewritten by it.
 */

import type { ReactNode } from 'react'

import { ApiError } from '../lib/api'

export const inputClass =
  'w-full rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm outline-none focus:border-neutral-500'

export const buttonClass =
  'rounded-lg bg-neutral-900 px-4 py-2 text-sm font-medium text-white disabled:opacity-50'

export const secondaryButtonClass =
  'rounded-lg border border-neutral-300 px-3 py-1.5 text-sm text-neutral-700 hover:bg-neutral-100 disabled:opacity-50'

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
    <p role="alert" className="rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">
      {message}
    </p>
  )
}

export function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <main className="mx-auto flex min-h-screen w-full max-w-sm flex-col justify-center gap-6 px-6">
      <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
      {children}
    </main>
  )
}
