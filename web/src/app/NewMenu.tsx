import { FolderPlus, FolderUp, Plus, Upload } from 'lucide-react'
import { useState } from 'react'
import { useLocation } from 'react-router'

import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

import { NewFolderDialog } from '../features/browser/NewFolderDialog'
import { openPicker } from '../features/upload/ui/pickers'
import { useCurrentFolder } from './CurrentFolder'

/**
 * The one button that adds something.
 *
 * Everything that puts bytes or a folder into the Drive is behind it, in the
 * rail, on every screen — rather than three controls that used to sit on the
 * folder page and vanish the moment you searched for something.
 *
 * All three land in `useCurrentFolder()` — the menu reads the destination, it
 * does not decide it — and the two uploads hand it to the picker at the click,
 * because that is the last moment it is still certainly the folder the person
 * is looking at. What changes off a folder screen is only the wording: where
 * there is no "here", the items name where the files will actually go.
 *
 * Which screens have a "here" is an allowlist rather than a list of exceptions.
 * A screen that is not a folder is the norm, not the oddity, and a denylist
 * answers "here" for every screen nobody has thought about yet — which is the
 * one answer that can be a lie.
 */
export function NewMenu() {
  const folderId = useCurrentFolder()
  const [creating, setCreating] = useState(false)
  const { pathname } = useLocation()
  const here = pathname === '/' || pathname.startsWith('/folders/')

  return (
    <>
      <DropdownMenu modal={false}>
        <DropdownMenuTrigger asChild>
          {/* A raised white pill, not a teal one. Teal in this product means
              something is selected, in progress, or focused; spending it on a
              button that is on screen permanently would make the one colour
              that reports state into wallpaper. The shape carries the emphasis
              instead — the only rounded-full raised control in the chrome,
              hugging its label while the nav rows beneath it stretch. */}
          <Button
            variant="outline"
            size="lg"
            className="h-12 gap-3 self-start rounded-full border-line-strong px-6 text-[14px] text-ink shadow-card hover:border-line-strong hover:text-ink active:bg-line/50 active:shadow-none"
          >
            <Plus className="size-5 text-ink-2" />
            New
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" sideOffset={6} className="w-60 rounded-pop p-1.5 shadow-pop">
          <DropdownMenuItem onSelect={() => setCreating(true)}>
            <FolderPlus />
            New folder
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => openPicker('files', folderId)}>
            <Upload />
            {here ? 'Upload files' : 'Upload files to My Drive'}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => openPicker('folder', folderId)}>
            <FolderUp />
            {here ? 'Upload folder' : 'Upload folder to My Drive'}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      {/* A sibling of the menu, not a child of an item: an item unmounts as the
          menu closes, and would take the dialog it opened with it. */}
      <NewFolderDialog parentId={folderId} open={creating} onOpenChange={setCreating} />
    </>
  )
}
