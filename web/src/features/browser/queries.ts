/**
 * Queries and mutations for the file tree. Query keys live here so the upload
 * manager can invalidate a folder's children when an upload publishes into it.
 */

import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { createFolder, getNode, listChildren, trashNode, type DriveNode } from '../../lib/api'

export const childrenKey = (folderId: string) => ['children', folderId] as const
export const nodeKey = (id: string) => ['node', id] as const

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

export function useTrashNode(parentId: string) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => trashNode(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: childrenKey(parentId) })
      void client.invalidateQueries({ queryKey: ['trash'] })
    },
  })
}
