import { X } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { Outlet, useParams } from 'react-router'

import { Button } from '@/components/ui/button'
import { Sheet, SheetClose, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { TooltipProvider } from '@/components/ui/tooltip'

import { useSession } from '../features/auth/session'
import { UploadDock } from '../features/upload/ui/UploadDock'
import { UploadPickers } from '../features/upload/ui/pickers'
import { DriveMark } from '../ui/icons'
import { CurrentFolderProvider } from './CurrentFolder'
import { SideRail } from './SideRail'
import { TopBar } from './TopBar'

/**
 * The signed-in chrome: a bar across the top, a rail down the left, the screen
 * in the space that leaves.
 *
 * Both are `fixed`, so the list scrolls under them and the commands never
 * scroll away. The rail is the one piece with two shapes, and the layout is
 * careful to render only ever one of them.
 */
export function AppLayout() {
  const { id } = useParams()
  const session = useSession()
  const [drawer, setDrawer] = useState(false)

  return (
    <CurrentFolderProvider folderId={id ?? session.root_id}>
      <TooltipProvider delayDuration={400}>
        <div className="min-h-screen bg-canvas text-ink">
          <TopBar onOpenRail={() => setDrawer(true)} />

          {/* Exactly one rail exists at a time, and it is a mount rather than a
              breakpoint that guarantees it: two copies behind `hidden md:flex`
              would mean two "Trash" links in the accessibility tree and two
              answers to "where am I". The drawer only opens below `md`, where
              this one is display:none anyway, so unmounting it costs nothing. */}
          {!drawer && (
            <aside className="fixed top-14 bottom-0 left-0 z-30 hidden w-60 flex-col border-r border-line bg-surface md:flex">
              <SideRail />
            </aside>
          )}

          <Sheet open={drawer} onOpenChange={setDrawer}>
            <SheetContent side="left" className="w-72 gap-0" showCloseButton={false}>
              <SheetHeader className="flex-row items-center gap-2 border-b border-line px-3 py-2.5">
                <SheetTitle className="flex items-center gap-2 text-[15px] tracking-tight text-ink">
                  <span className="flex h-6 w-6 items-center justify-center rounded-md bg-ink text-canvas">
                    <DriveMark className="h-3.5 w-3.5" />
                  </span>
                  Drive
                </SheetTitle>
                <SheetDescription className="sr-only">Places in your Drive, and the New menu.</SheetDescription>
                <SheetClose asChild>
                  <Button variant="ghost" size="icon-sm" className="ml-auto" aria-label="Close navigation">
                    <X />
                  </Button>
                </SheetClose>
              </SheetHeader>
              <SideRail onNavigate={() => setDrawer(false)} />
            </SheetContent>
          </Sheet>

          <div className="pt-14 md:pl-60">
            <Outlet />
          </div>

          {/* Mounted once, here: the engine outlives route changes, and so must
              the pickers that feed it and the manager that shows what it is
              doing. */}
          <UploadPickers />
          <DockStack>
            <UploadDock />
          </DockStack>
        </div>
      </TooltipProvider>
    </CurrentFolderProvider>
  )
}

/**
 * The bottom-right corner, where the app reports on work it is doing in the
 * background. A stack rather than a slot: anything else that has to report from
 * down here stacks above the uploads instead of landing on top of them, which
 * is what two independently-`fixed` panels in one corner would do.
 *
 * `pointer-events-none` on the stack and `auto` on each child keeps the empty
 * corner clickable — the container spans the full width below `sm`.
 */
function DockStack({ children }: { children: ReactNode }) {
  return (
    <div className="pointer-events-none fixed inset-x-3 bottom-3 z-40 flex flex-col gap-3 sm:inset-x-auto sm:right-5 sm:bottom-5 sm:w-[26rem]">
      {children}
    </div>
  )
}
