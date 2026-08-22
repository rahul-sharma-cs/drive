import { createContext, useContext, type ReactNode } from 'react'

const CurrentFolder = createContext<string | null>(null)

/**
 * Which folder a command lands in.
 *
 * The commands that create things — New folder, Upload files, Upload folder —
 * used to live on the folder screen, where "here" was a prop. They now live in
 * the rail, which outlives every navigation and has no idea what is on screen,
 * so the answer has to come from somewhere both sides can read.
 *
 * The layout provides it once, from the route: a layout-route match shares the
 * matched branch's params object, so `useParams().id` is the `:id` of the
 * folder route underneath it. Off a folder screen — search, trash — there is no
 * "here" and the answer is the root, which is where those screens' commands say
 * they will put things.
 */
export function CurrentFolderProvider({ folderId, children }: { folderId: string; children: ReactNode }) {
  return <CurrentFolder.Provider value={folderId}>{children}</CurrentFolder.Provider>
}

export function useCurrentFolder(): string {
  const folderId = useContext(CurrentFolder)
  if (folderId === null) throw new Error('useCurrentFolder() used outside the app layout')
  return folderId
}
