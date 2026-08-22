import { Menu } from 'lucide-react'
import { Link } from 'react-router'

import { Button } from '@/components/ui/button'

import { DriveMark } from '../ui/icons'
import { AccountMenu } from './AccountMenu'
import { HeaderSearch } from './HeaderSearch'

/**
 * The top bar: mark, search, face.
 *
 * Search sits in a column of its own so it lands in the middle of the window
 * rather than wherever the two flanking blocks leave room — the flanks are
 * pinned to the rail's width from `md` up, which is what makes the centring
 * exact and lines the mark up with the rail underneath it.
 */
export function TopBar({ onOpenRail }: { onOpenRail: () => void }) {
  return (
    <header className="fixed inset-x-0 top-0 z-40 flex h-14 items-center gap-2 border-b border-line bg-surface px-2 sm:px-4">
      <div className="flex shrink-0 items-center gap-1 md:w-60 md:pr-3">
        <Button
          variant="ghost"
          size="icon-sm"
          className="md:hidden"
          aria-label="Open navigation"
          onClick={onOpenRail}
        >
          <Menu />
        </Button>
        <Link
          to="/"
          className="flex items-center gap-2 rounded-control px-1.5 py-1.5 text-[15px] font-semibold tracking-tight whitespace-nowrap"
        >
          <span className="flex h-6 w-6 items-center justify-center rounded-md bg-ink text-canvas">
            <DriveMark className="h-3.5 w-3.5" />
          </span>
          {/* The wordmark costs ~46px the top bar does not have at 390px; the
              mark carries it there, and the name stays for screen readers. */}
          <span className="sr-only sm:not-sr-only">Drive</span>
        </Link>
      </div>

      <div className="mx-auto w-full max-w-xl min-w-0">
        <HeaderSearch />
      </div>

      <div className="flex shrink-0 items-center justify-end md:w-60 md:pl-3">
        <AccountMenu />
      </div>
    </header>
  )
}
