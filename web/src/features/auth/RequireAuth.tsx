import { Navigate, Outlet } from 'react-router'

import { useMe } from './session'

/** Route guard: everything behind it can call `useSession()` for a real user. */
export function RequireAuth() {
  const { data, isPending, error } = useMe()

  if (isPending) {
    return <p className="p-6 text-sm text-neutral-500">Loading…</p>
  }
  if (error) {
    return (
      <p role="alert" className="p-6 text-sm text-red-700">
        Drive is unreachable right now. Reload to try again.
      </p>
    )
  }
  if (!data) {
    return <Navigate to="/login" replace />
  }
  return <Outlet />
}
