import { HardDrive, Trash2 } from 'lucide-react'
import type { ReactNode } from 'react'
import { NavLink, useLocation } from 'react-router'

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
 */
export function SideRail({ onNavigate }: { onNavigate?: () => void }) {
  const { pathname } = useLocation()

  return (
    <div className="flex flex-1 flex-col gap-5 overflow-y-auto px-3 py-4">
      {/* Nothing is created in the trash and nothing is uploaded into it, so the
          command that would have to land somewhere else is not offered there. */}
      {pathname !== '/trash' && <NewMenu />}

      <nav aria-label="Places" className="flex flex-col gap-0.5">
        <RailLink to="/" end icon={<HardDrive />} label="My Drive" onNavigate={onNavigate} />
        <RailLink to="/trash" icon={<Trash2 />} label="Trash" onNavigate={onNavigate} />
      </nav>

      <div className="mt-auto border-t border-line pt-4">
        <StorageMeter />
      </div>
    </div>
  )
}

/** A destination, named for what is in it rather than for where it sits. */
function RailLink({
  to,
  end,
  icon,
  label,
  onNavigate,
}: {
  to: string
  end?: boolean
  icon: ReactNode
  label: string
  onNavigate?: () => void
}) {
  return (
    <NavLink
      to={to}
      end={end}
      onClick={onNavigate}
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
