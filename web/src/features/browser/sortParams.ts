/**
 * Which column a folder is sorted by, kept in the URL.
 *
 * In the URL rather than in component state for the same reason the search
 * query is: a sorted folder is a place. It survives a reload, it is in the back
 * button, and it is what gets pasted to somebody else. It also rides whatever
 * else is on the route — the preview key, a search's `q` — because the params
 * are copied rather than rebuilt.
 *
 * Anything the URL says that is not one of the three keys is ignored and the
 * default stands: the value is going straight back out in a request, and a
 * folder that refuses to load because someone edited its address is worse than
 * one that quietly sorts by name.
 */

import { useCallback } from 'react'
import { useSearchParams } from 'react-router'

import { DEFAULT_SORT, type Sort, type SortDir, type SortKey } from '../../lib/api'

const KEYS: readonly SortKey[] = ['name', 'updated_at', 'size']

export function readSort(params: URLSearchParams): Sort {
  const key = params.get('sort')
  if (!KEYS.includes(key as SortKey)) return DEFAULT_SORT
  return { key: key as SortKey, dir: params.get('dir') === 'desc' ? 'desc' : 'asc' }
}

export interface SortControl {
  sort: Sort
  /**
   * Clicking a column: the one already sorted on flips direction, any other
   * takes over ascending — which is what every file manager does, and what
   * makes a second click on the same header mean "the other way round".
   */
  toggle: (key: SortKey) => void
}

export function useSort(): SortControl {
  const [params, setParams] = useSearchParams()
  const sort = readSort(params)

  const toggle = useCallback(
    (key: SortKey) => {
      const dir: SortDir = key === sort.key && sort.dir === 'asc' ? 'desc' : 'asc'
      const next = new URLSearchParams(params)
      next.set('sort', key)
      next.set('dir', dir)
      // Replace, not push: ten clicks on a header are one act of looking at a
      // folder, and Back should leave the folder rather than walk them again.
      setParams(next, { replace: true })
    },
    [params, setParams, sort.key, sort.dir],
  )

  return { sort, toggle }
}
