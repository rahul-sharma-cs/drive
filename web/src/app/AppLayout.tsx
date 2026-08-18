import { useMutation } from '@tanstack/react-query'
import { Link, NavLink, Outlet, useNavigate } from 'react-router'

import { logout } from '../lib/api'
import { ghostButtonClass } from '../ui/controls'
import { DriveMark, FolderIcon, TrashIcon } from '../ui/icons'
import { useSession, useSetSession } from '../features/auth/session'
import { UploadDock } from '../features/upload/ui/UploadDock'
import { HeaderSearch } from './HeaderSearch'
import { StorageMeter } from './StorageMeter'

/**
 * The signed-in chrome.
 *
 * One element, two shapes: a rail down the left on a wide screen, the same
 * contents as a bar across the top on a narrow one. Same DOM either way — a
 * second copy behind a breakpoint would mean two "Trash" links on the page and
 * two answers to every "where am I".
 */
export function AppLayout() {
  const user = useSession()
  const navigate = useNavigate()
  const setSession = useSetSession()

  const signOut = useMutation({
    mutationFn: logout,
    onSuccess: () => {
      setSession(null)
      void navigate('/login', { replace: true })
    },
  })

  return (
    <div className="min-h-screen bg-canvas text-ink">
      <header
        className="material sticky top-0 z-30 flex flex-wrap items-center gap-x-1 gap-y-2 border-b border-line px-3 py-2
                   md:fixed md:inset-y-0 md:left-0 md:w-60 md:flex-col md:flex-nowrap md:items-stretch md:gap-1
                   md:border-r md:border-b-0 md:bg-surface md:px-3 md:py-4 md:backdrop-blur-none"
      >
        <Link
          to="/"
          className="flex shrink-0 items-center gap-2 rounded-control px-2 py-1.5 text-[15px] font-semibold tracking-tight whitespace-nowrap"
        >
          <span className="flex h-6 w-6 items-center justify-center rounded-md bg-ink text-canvas">
            <DriveMark className="h-3.5 w-3.5" />
          </span>
          {/* The wordmark costs ~46px the top bar does not have at 390px; the
              mark carries it there, and the name stays for screen readers. */}
          <span className="sr-only md:not-sr-only">Drive</span>
        </Link>

        <nav className="flex shrink-0 items-center gap-1 md:order-3 md:mt-4 md:flex-col md:items-stretch">
          <NavItem to="/" end icon={<FolderIcon />} label="My Drive" />
          <NavItem to="/trash" icon={<TrashIcon />} label="Trash" />
        </nav>

        {/* Its own row on a narrow screen, under the rail's mark on a wide one:
            either way it is present on every screen rather than being a place
            you have to go to. */}
        <div className="order-last w-full md:order-2 md:mt-3 md:w-auto">
          <HeaderSearch />
        </div>

        <div className="ml-auto flex shrink-0 items-center gap-1 md:order-4 md:mt-auto md:ml-0 md:flex-col md:items-stretch md:gap-3">
          <div className="hidden md:block">
            <StorageMeter />
          </div>
          <div className="hidden border-t border-line pt-3 md:block md:pt-0">
            <span className="block truncate px-2 pt-3 text-[13px] text-ink-3" title={user.email}>
              {user.email}
            </span>
          </div>
          <button
            className={`${ghostButtonClass} whitespace-nowrap md:justify-start md:px-2`}
            onClick={() => signOut.mutate()}
            disabled={signOut.isPending}
          >
            Sign out
          </button>
        </div>
      </header>

      <div className="md:pl-60">
        <Outlet />
      </div>

      {/* Mounted once, here: the engine outlives route changes, and so must the
          manager that shows what it is doing. */}
      <UploadDock />
    </div>
  )
}

/** A destination, named for what is in it rather than for where it sits. */
function NavItem({ to, end, icon, label }: { to: string; end?: boolean; icon: React.ReactNode; label: string }) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        `flex items-center gap-2 rounded-control px-2.5 py-1.5 text-[13px] font-medium whitespace-nowrap transition duration-100 ${
          isActive ? 'bg-accent-soft text-accent-strong' : 'text-ink-2 hover:bg-surface-muted hover:text-ink'
        }`
      }
    >
      <span className="shrink-0 text-current">{icon}</span>
      {label}
    </NavLink>
  )
}
