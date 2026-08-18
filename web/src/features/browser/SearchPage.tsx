import { useQuery } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router'

import { downloadHref, search } from '../../lib/api'
import { Card, EmptyState, ghostButtonClass, SkeletonRows } from '../../ui/controls'
import { DownloadIcon, FileIcon, FolderIcon, SearchIcon } from '../../ui/icons'
import { formatBytes } from '../../ui/format'
import { formatWhen } from '../../ui/when'

/**
 * Search results. `q` only — the filter chips and the size/date parameters the
 * API also accepts are cut for now, and half a filter UI is worse than none.
 *
 * The query lives in the URL, not in this component: the box in the chrome
 * navigates here, which makes a search a real location — shareable, in the
 * back button, and still there after a reload.
 */
export function SearchPage() {
  const [params] = useSearchParams()
  const q = (params.get('q') ?? '').trim()

  const results = useQuery({
    queryKey: ['search', q],
    queryFn: () => search(q),
    enabled: q !== '',
  })

  const items = results.data?.items ?? []

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <h1 className="text-[17px] font-semibold tracking-tight">
        {q === '' ? 'Search' : <>Results for “{q}”</>}
      </h1>

      <Card>
        {q === '' && (
          <EmptyState
            icon={<SearchIcon />}
            title="Search across every folder"
            hint="Start typing in the box above — matching is on the name, anywhere in it."
          />
        )}
        {q !== '' && (
          <>
            {results.isPending && <SkeletonRows rows={2} />}
            {results.error && (
              <div role="alert" className="px-4 py-6 text-sm text-ink-2">
                That search didn’t run. Try it again in a moment.
              </div>
            )}
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
                        className="min-w-0 flex-1 truncate text-sm font-medium text-ink hover:underline"
                        to={`/folders/${node.id}`}
                      >
                        {node.name}
                      </Link>
                    ) : (
                      <span className="min-w-0 flex-1 truncate text-sm text-ink">{node.name}</span>
                    )}
                    <span className="numeric hidden w-28 shrink-0 text-ink-3 sm:block">
                      {formatWhen(node.updated_at)}
                    </span>
                    <span className="numeric w-16 shrink-0 text-right text-ink-3">
                      {node.size === null ? 'Folder' : formatBytes(node.size)}
                    </span>
                    <div className="flex shrink-0 items-center justify-end gap-1 sm:w-16">
                      {/* A result is out of context by definition, so getting
                          to the folder it lives in is a real action here. */}
                      {node.parent_id && (
                        <Link
                          className={ghostButtonClass}
                          to={`/folders/${node.parent_id}`}
                          aria-label={`Open the folder ${node.name} is in`}
                        >
                          <FolderIcon />
                        </Link>
                      )}
                      {node.kind === 'file' && (
                        <a
                          className={ghostButtonClass}
                          href={downloadHref(node.id)}
                          target="_blank"
                          rel="noopener"
                          aria-label={`Download ${node.name}`}
                        >
                          <DownloadIcon />
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
