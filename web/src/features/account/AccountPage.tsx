import { Separator } from '@/components/ui/separator'

import { IdentitiesSection } from './IdentitiesSection'
import { PasswordSection } from './PasswordSection'
import { ProfileSection } from './ProfileSection'
import { SessionsSection } from './SessionsSection'

/**
 * `/account` — the three things a person owns about their account: the name it
 * shows, the password it opens with, and the browsers it is open in.
 *
 * One column, hairline rules between the sections rather than a card apiece: a
 * settings screen is a list of separate concerns, and nesting each in its own
 * panel inside the page's panel makes it read as three unrelated screens
 * stacked. Each section owns its own form and its own mutation, so a failed
 * password change never touches the name above it.
 */
export function AccountPage() {
  return (
    <main className="mx-auto flex w-full max-w-2xl flex-col px-4 py-6 sm:px-6 sm:py-10">
      <header className="flex flex-col gap-1">
        <h1 className="text-[17px] font-semibold tracking-tight text-ink">Account</h1>
        <p className="text-[13px] text-ink-3">Your details, your password, and the devices holding a session.</p>
      </header>

      <Separator className="my-8" />
      <ProfileSection />
      <Separator className="my-8" />
      <PasswordSection />
      <Separator className="my-8" />
      <IdentitiesSection />
      <Separator className="my-8" />
      <SessionsSection />
    </main>
  )
}
