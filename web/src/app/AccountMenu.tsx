import { useMutation } from '@tanstack/react-query'
import { LogOut, Settings } from 'lucide-react'
import { Link, useNavigate } from 'react-router'

import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

import { useSession, useSetSession } from '../features/auth/session'
import { logout } from '../lib/api'

/**
 * Who is signed in, and the one thing you can do about it.
 *
 * Sign out used to be a button sitting in the rail beside My Drive and Trash,
 * as though leaving were a place you could go. It belongs here, behind the
 * face: identity and the account's own commands in one menu, which is also
 * where the account settings screen hangs off.
 *
 * The menu is non-modal on purpose — a modal Radix menu leaves
 * `pointer-events: none` on <body> for as long as it takes to unmount, and this
 * app's page behind is a drop target.
 */
export function AccountMenu() {
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
    <DropdownMenu modal={false}>
      <DropdownMenuTrigger
        aria-label="Your account"
        className="rounded-full transition duration-100 hover:opacity-85 active:scale-[0.97]"
      >
        <Avatar>
          <AvatarFallback className="bg-ink text-[12px] font-semibold text-canvas">
            {initials(user.display_name, user.email)}
          </AvatarFallback>
        </Avatar>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" sideOffset={8} className="w-64 rounded-pop p-1.5 shadow-pop">
        <div className="px-2 py-1.5">
          <p className="truncate text-[13px] font-medium text-ink">{user.display_name}</p>
          <p className="truncate text-[12px] text-ink-3" title={user.email}>
            {user.email}
          </p>
        </div>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/account">
            <Settings />
            Account settings
          </Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => signOut.mutate()} disabled={signOut.isPending}>
          <LogOut />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/**
 * One letter from a single name, two from a full one — the same rule every
 * address book uses, so the circle reads as a person rather than a hash. An
 * account with a blank display name falls back to its address, which the server
 * guarantees is there.
 */
function initials(displayName: string, email: string): string {
  const words = displayName.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return email.slice(0, 1).toUpperCase()
  const last = words.length > 1 ? words[words.length - 1] : ''
  return (words[0].slice(0, 1) + last.slice(0, 1)).toUpperCase()
}
