/**
 * Query keys and reads for share links. The keys follow `browser/queries.ts`:
 * `['shares']` is a prefix every share mutation invalidates, so the dialog's
 * one-file read and `/shared`'s list can never disagree about whether a link
 * exists. Nothing here touches a folder listing — a share changes no folder.
 */

import { useInfiniteQuery, useQuery } from '@tanstack/react-query'

import { getShareMeta, listShares } from '../../lib/api'

/** `/shared` — every active link, newest first. */
export const sharesKey = ['shares'] as const

/** The dialog's read: this file's one active link, or null. */
export const shareForNode = (nodeId: string) => ['shares', 'node', nodeId] as const

/** A recipient's `/meta` answer. A token and a node id never meet under one prefix. */
export const shareMeta = (token: string) => ['share', token] as const

export function useShares() {
  return useInfiniteQuery({
    queryKey: sharesKey,
    queryFn: ({ pageParam }) => listShares(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
}

/**
 * One file has at most one active link, so the narrowed list is read as a
 * single answer: the share, or `null` for "not shared".
 */
export function useShareForNode(nodeId: string) {
  return useQuery({
    queryKey: shareForNode(nodeId),
    queryFn: async () => (await listShares(undefined, nodeId)).items[0] ?? null,
  })
}

/** `retry: false` is the app-wide default too, but this one must never retry a 404. */
export function useShareMeta(token: string) {
  return useQuery({
    queryKey: shareMeta(token),
    queryFn: () => getShareMeta(token),
    retry: false,
  })
}
