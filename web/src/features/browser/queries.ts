/**
 * Queries and mutations for the file tree. Query keys live here so the upload
 * manager can invalidate a folder's children when an upload publishes into it.
 */

import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import {
  copyNode,
  createFolder,
  getNode,
  getUsage,
  listChildren,
  trashNode,
  updateNode,
  type DriveNode,
} from '../../lib/api'

export const childrenKey = (folderId: string) => ['children', folderId] as const

/**
 * Which folder listings a mutation invalidates. A screen that knows the folder
 * it is looking at names it; search results span every folder, so a mutation
 * there re-reads all of them rather than guessing at one.
 */
const childrenScope = (parentId: string | undefined) =>
  parentId === undefined ? (['children'] as const) : childrenKey(parentId)
export const nodeKey = (id: string) => ['node', id] as const
export const usageKey = ['usage'] as const

export function useChildren(folderId: string) {
  return useInfiniteQuery({
    queryKey: childrenKey(folderId),
    queryFn: ({ pageParam }) => listChildren(folderId, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
}

/**
 * The chain from the root down to `folderId`, walked one parent at a time.
 *
 * The API has no path endpoint — a node knows only its parent — so a hard
 * refresh deep in the tree has to walk up. Depth is small, and every hop is a
 * `getNode` the cache already holds after a click-through.
 */
export function useBreadcrumbs(folderId: string) {
  return useQuery({
    queryKey: ['breadcrumbs', folderId],
    queryFn: async (): Promise<DriveNode[]> => {
      const chain: DriveNode[] = []
      let id: string | null = folderId
      while (id) {
        const node: DriveNode = await getNode(id)
        chain.unshift(node)
        id = node.parent_id
      }
      return chain
    },
  })
}

export function useCreateFolder(parentId: string) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => createFolder(parentId, name),
    onSuccess: () => client.invalidateQueries({ queryKey: childrenKey(parentId) }),
  })
}

export function useTrashNode(parentId?: string) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => trashNode(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: childrenScope(parentId) })
      void client.invalidateQueries({ queryKey: ['trash'] })
      // A trashed node has to leave open search results too — nothing else
      // re-reads them, and the row would otherwise sit there offering actions
      // on something that is now in the trash.
      void client.invalidateQueries({ queryKey: ['search'] })
    },
  })
}

/**
 * Rename and move are one endpoint, so they are one mutation. Both invalidate
 * the folder on screen; a move additionally invalidates the destination, which
 * is a folder this screen is not showing but may be one click away.
 */
export function useUpdateNode(parentId?: string) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({
      id,
      ...patch
    }: {
      id: string
      name?: string
      parent_id?: string
      conflict_policy?: 'rename' | 'replace'
    }) => updateNode(id, patch),
    onSuccess: (node, vars) => {
      void client.invalidateQueries({ queryKey: childrenScope(parentId) })
      if (vars.parent_id) void client.invalidateQueries({ queryKey: childrenKey(vars.parent_id) })
      // A rename changes what the breadcrumb for that folder says, and search
      // results hold the old name too.
      void client.invalidateQueries({ queryKey: ['breadcrumbs'] })
      void client.invalidateQueries({ queryKey: ['search'] })
      void client.invalidateQueries({ queryKey: nodeKey(node.id) })
    },
  })
}

/** Files only. The server answers a folder with 422 `unsupported`. */
export function useCopyNode(parentId?: string) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({
      id,
      destination,
      conflictPolicy,
    }: {
      id: string
      destination: string
      conflictPolicy?: 'rename' | 'replace'
    }) => copyNode(id, destination, conflictPolicy),
    onSuccess: (_node, vars) => {
      void client.invalidateQueries({ queryKey: childrenScope(parentId) })
      void client.invalidateQueries({ queryKey: childrenKey(vars.destination) })
      void client.invalidateQueries({ queryKey: usageKey })
    },
  })
}

/**
 * How much of the quota this account is using. Refetched on mount rather than
 * polled: it only moves when this tab does something, and every one of those
 * things already invalidates it.
 */
export function useUsage() {
  return useQuery({ queryKey: usageKey, queryFn: getUsage, staleTime: 30_000 })
}
