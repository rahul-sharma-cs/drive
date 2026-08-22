/**
 * Queries and mutations for the file tree. Query keys live here so the upload
 * manager can invalidate a folder's children when an upload publishes into it.
 */

import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import {
  BULK_LIMIT,
  copyNode,
  createFolder,
  emptyTrash,
  getNode,
  getUsage,
  listChildren,
  purgeNodes,
  restoreNodes,
  trashNode,
  updateNode,
  type BulkAnswer,
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

/* -------------------------------------------------------------- the trash */

/** What a bulk restore or purge did to the rows it was handed. */
export interface BulkOutcome {
  /** Left in the trash: something already holds that name where they came from. */
  conflicts: DriveNode[]
  /** The server could not do it. */
  failed: DriveNode[]
  /** It stopped getting through the list. The rest are still in the trash. */
  stalled: boolean
}

/**
 * One bulk call, then the rest, until the list is spent.
 *
 * Two things make this a loop rather than a request. The server takes at most
 * `BULK_LIMIT` ids at a time, and it works under a wall-clock budget — when the
 * budget runs out mid-list the ids it never reached come back as `pending` and
 * it is the client's job to ask again. Statuses that mean "no longer in the
 * trash" (`ok`, and `not_found`, which is the same thing arrived at earlier)
 * need no reporting; the two that do are collected for the screen to say out
 * loud.
 */
async function runBulk(
  call: (ids: string[]) => Promise<BulkAnswer>,
  nodes: readonly DriveNode[],
): Promise<BulkOutcome> {
  const byId = new Map(nodes.map((node) => [node.id, node]))
  const conflicts: DriveNode[] = []
  const failed: DriveNode[] = []
  let queue = nodes.map((node) => node.id)
  let stalled = false

  while (queue.length > 0) {
    const batch = queue.slice(0, BULK_LIMIT)
    const rest = queue.slice(BULK_LIMIT)
    const answer = await call(batch)

    const again: string[] = []
    for (const result of answer.results) {
      const node = byId.get(result.id)
      if (node === undefined) continue
      if (result.status === 'pending') again.push(result.id)
      else if (result.status === 'name_conflict') conflicts.push(node)
      else if (result.status === 'error') failed.push(node)
    }

    // The server gets to at least one root per call, so a round that resolved
    // nothing at all would resolve nothing on the next pass either. Trusting
    // `remaining` alone here is what would spin the browser against a server
    // in that state.
    if (again.length === batch.length) {
      stalled = true
      break
    }
    queue = [...again, ...rest]
  }

  return { conflicts, failed, stalled }
}

/**
 * What every trash mutation makes stale, in one place: a restored node is back
 * in a folder listing and back in search results, a purged one has given its
 * bytes to the quota, and either way the trash itself has changed.
 */
function useTrashRefresh() {
  const client = useQueryClient()
  return () => {
    void client.invalidateQueries({ queryKey: ['trash'] })
    void client.invalidateQueries({ queryKey: ['children'] })
    void client.invalidateQueries({ queryKey: ['search'] })
    void client.invalidateQueries({ queryKey: usageKey })
  }
}

export function useRestoreNodes() {
  const refresh = useTrashRefresh()
  return useMutation({
    mutationFn: (nodes: readonly DriveNode[]) => runBulk(restoreNodes, nodes),
    onSuccess: refresh,
  })
}

export function usePurgeNodes() {
  const refresh = useTrashRefresh()
  return useMutation({
    mutationFn: (nodes: readonly DriveNode[]) => runBulk(purgeNodes, nodes),
    onSuccess: refresh,
  })
}

/**
 * Empty the trash — all of it, not just the page that is loaded. The route
 * takes a batch of roots per call and says whether more are left; a call that
 * purged nothing has stopped making progress, and looping on `remaining` past
 * that point would never end.
 */
export function useEmptyTrash() {
  const refresh = useTrashRefresh()
  return useMutation({
    mutationFn: async () => {
      let purged = 0
      for (;;) {
        const round = await emptyTrash()
        purged += round.purged
        if (!round.remaining || round.purged === 0) return { purged, stalled: round.remaining }
      }
    },
    onSuccess: refresh,
  })
}
