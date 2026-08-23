import { HardDrive, Link2, Trash2 } from 'lucide-react'
import type { ReactNode } from 'react'
import { NavLink } from 'react-router'

import { NewMenu } from './NewMenu'
import { StorageMeter } from './StorageMeter'

/**
 * The left rail: what you can add, where you can go, and how much room is left,
 * in that order — the order a person asks those questions in.
 *
 * One component, two homes. Wide screens fix it down the left; narrow ones put
 * the same element inside a drawer. The layout renders exactly one of the two,
 * so there is never a second "Trash" link on the page to disagree with the
 * first about which one is current.
 *
 * Nothing here knows it is in a drawer, and nothing here closes one: the layout
 * closes it on the route change these links cause, which also covers the routes
 * they don't — Back, a redirect, a link on the screen behind.
 */
export function SideRail() {
  return (
    <div className="flex flex-1 flex-col gap-5 overflow-y-auto px-3 py-4">
      {/* On every screen, the trash and the account settings included. A screen
          that nothing lands in does not take the command away, it renames it:
          the menu reads "to My Drive" there and puts things exactly there.
          Hiding it instead pulled the destinations below it up by the height of
          a button, so walking into the trash and back out moved the link the
          person was aiming at. */}
      <NewMenu />

      <nav aria-label="Places" className="flex flex-col gap-0.5">
        <RailLink to="/" end icon={<HardDrive />} label="My Drive" />
        <RailLink to="/shared" icon={<Link2 />} label="Shared links" />
        <RailLink to="/trash" icon={<Trash2 />} label="Trash" />
      </nav>

      <div className="mt-auto border-t border-line pt-4">
        <StorageMeter />
      </div>
    </div>
  )
}

/** A destination, named for what is in it rather than for where it sits. */
function RailLink({ to, end, icon, label }: { to: string; end?: boolean; icon: ReactNode; label: string }) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        `flex items-center gap-3 rounded-full px-3 py-2 text-[14px] font-medium whitespace-nowrap transition duration-100 [&_svg]:size-[18px] [&_svg]:shrink-0 ${
          isActive ? 'bg-teal-soft text-teal-strong' : 'text-ink-2 hover:bg-surface-muted hover:text-ink'
        }`
      }
    >
      {icon}
      {label}
    </NavLink>
  )
}
