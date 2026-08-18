import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { Link } from 'react-router'

import { downloadHref, search } from '../../lib/api'
import { inputClass, secondaryButtonClass } from '../../ui/controls'
import { formatBytes } from '../../ui/format'

/**
 * Name search. `q` only — the filter chips and the size/date parameters the API
 * also accepts are a Tier 1 cut, and half a filter UI is worse than none.
 */
export function SearchPage() {
  const [text, setText] = useState('')
  const q = useDebounced(text, 250)

  const results = useQuery({
    queryKey: ['search', q],
    queryFn: () => search(q),
    enabled: q.trim() !== '',
  })

  const items = results.data?.items ?? []

  return (
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-6 py-6">
      <label className="flex flex-col gap-1 text-sm">
        Search by name
        <input className={inputClass} value={text} onChange={(e) => setText(e.target.value)} autoFocus />
      </label>

      {q.trim() !== '' && (
        <section className="rounded-xl border border-neutral-200 bg-white">
          {results.isPending && <p className="p-4 text-sm text-neutral-500">Searching…</p>}
          {results.isSuccess && items.length === 0 && (
            <p className="p-4 text-sm text-neutral-500">Nothing matches “{q}”.</p>
          )}
          <ul>
            {items.map((node) => (
              <li
                key={node.id}
                className="flex items-center gap-3 border-b border-neutral-100 px-4 py-2.5 last:border-b-0"
              >
                {node.kind === 'folder' ? (
                  <Link className="text-sm font-medium hover:underline" to={`/folders/${node.id}`}>
                    {node.name}
                  </Link>
                ) : (
                  <span className="text-sm">{node.name}</span>
                )}
                <span className="ml-auto text-xs text-neutral-500">
                  {node.size === null ? 'Folder' : formatBytes(node.size)}
                </span>
                {node.kind === 'file' && (
                  <a className={secondaryButtonClass} href={downloadHref(node.id)}>
                    Download
                  </a>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}
    </main>
  )
}

/** Keeps a keystroke from becoming a query — the server does ILIKE per call. */
function useDebounced(value: string, ms: number): string {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const handle = setTimeout(() => setSettled(value), ms)
    return () => clearTimeout(handle)
  }, [value, ms])
  return settled
}
