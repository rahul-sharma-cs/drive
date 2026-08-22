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
 * All three land in `useCurrentFolder()`, so the menu itself holds no notion of
 * a destination. What changes off a folder screen is only the wording: on
 * search results there is no "here", so the items name where the files will
 * actually go.
 */
export function NewMenu() {
  const folderId = useCurrentFolder()
  const [creating, setCreating] = useState(false)
  const here = useLocation().pathname !== '/search'

  return (
    <>
      <DropdownMenu modal={false}>
        <DropdownMenuTrigger asChild>
          <Button
            size="lg"
            className="h-12 w-full justify-start gap-3 rounded-full px-4 text-[14px] shadow-card hover:bg-teal-strong"
          >
            <Plus className="size-5" />
            New
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" sideOffset={6} className="w-60 rounded-pop p-1.5 shadow-pop">
          <DropdownMenuItem onSelect={() => setCreating(true)}>
            <FolderPlus />
            New folder
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => openPicker('files')}>
            <Upload />
            {here ? 'Upload files' : 'Upload files to My Drive'}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => openPicker('folder')}>
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
