import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { Link } from 'react-router'

import { downloadHref, search } from '../../lib/api'
import { Card, EmptyState, fieldClass, ghostButtonClass, inputClass, SkeletonRows } from '../../ui/controls'
import { DownloadIcon, FileIcon, FolderIcon, SearchIcon } from '../../ui/icons'
import { formatBytes } from '../../ui/format'

/**
 * Name search. `q` only — the filter chips and the size/date parameters the API
 * also accepts are a Tier 1 cut, and half a filter UI is worse than none.
 */
export function SearchPage() {
  const [text, setText] = useState('')
  const q = useDebounced(text.trim(), 250)

  const results = useQuery({
    queryKey: ['search', q],
    queryFn: () => search(q),
    enabled: q.trim() !== '',
  })

  const items = results.data?.items ?? []

  return (
    <main className="mx-auto flex w-full max-w-4xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <label className={fieldClass}>
        Search by name
        <input
          className={inputClass}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="report, invoice, .mov…"
          autoFocus
        />
      </label>

      <Card>
        {q.trim() === '' && (
          <EmptyState
            icon={<SearchIcon />}
            title="Search across every folder"
            hint="Matching is on the name, anywhere in it — results appear as you type."
          />
        )}
        {q.trim() !== '' && (
          <>
            {results.isPending && <SkeletonRows rows={2} />}
            {results.isSuccess && items.length === 0 && (
              <EmptyState icon={<SearchIcon />} title={`Nothing matches “${q}”.`} hint="Try a shorter piece of the name." />
            )}
            {items.length > 0 && (
              <ul className="divide-y divide-line">
                {items.map((node) => (
                  <li
                    key={node.id}
                    className="flex items-center gap-3 px-3 py-2.5 transition duration-100 hover:bg-surface-muted sm:px-4"
                  >
                    <span className={node.kind === 'folder' ? 'shrink-0 text-accent' : 'shrink-0 text-ink-3'}>
                      {node.kind === 'folder' ? <FolderIcon /> : <FileIcon />}
                    </span>
                    {node.kind === 'folder' ? (
                      <Link
                        className="min-w-0 truncate text-sm font-medium text-ink hover:underline"
                        to={`/folders/${node.id}`}
                      >
                        {node.name}
                      </Link>
                    ) : (
                      <span className="min-w-0 truncate text-sm text-ink">{node.name}</span>
                    )}
                    <span className="numeric ml-auto w-16 shrink-0 text-right text-ink-3">
                      {node.size === null ? 'Folder' : formatBytes(node.size)}
                    </span>
                    <div className="flex shrink-0 items-center justify-end gap-1 sm:w-[7.5rem]">
                      {node.kind === 'file' && (
                        <a className={ghostButtonClass} href={downloadHref(node.id)} target="_blank" rel="noopener">
                          <DownloadIcon />
                          <span className="sr-only sm:not-sr-only">Download</span>
                        </a>
                      )}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </Card>
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
