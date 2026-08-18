import { useMutation } from '@tanstack/react-query'
import { Link, Outlet, useNavigate } from 'react-router'

import { logout } from '../lib/api'
import { secondaryButtonClass } from '../ui/controls'
import { useSession, useSetSession } from '../features/auth/session'
import { UploadDock } from '../features/upload/ui/UploadDock'

/** The signed-in chrome: one header, one outlet. */
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
    <div className="min-h-screen bg-neutral-50 text-neutral-900">
      <header className="flex items-center gap-4 border-b border-neutral-200 bg-white px-6 py-3">
        <Link to="/" className="text-base font-semibold tracking-tight">
          Drive
        </Link>
        <nav className="flex gap-4 text-sm text-neutral-600">
          <Link to="/search">Search</Link>
          <Link to="/trash">Trash</Link>
        </nav>
        <span className="ml-auto text-sm text-neutral-500">{user.email}</span>
        <button className={secondaryButtonClass} onClick={() => signOut.mutate()} disabled={signOut.isPending}>
          Sign out
        </button>
      </header>
      <Outlet />
      {/* Mounted once, here: the engine outlives route changes, and so must the
          manager that shows what it is doing. */}
      <UploadDock />
    </div>
  )
}
