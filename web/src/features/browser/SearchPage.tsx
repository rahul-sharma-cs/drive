import { useQuery } from '@tanstack/react-query'
import { FolderOpen, Search } from 'lucide-react'
import { Link, useSearchParams } from 'react-router'

import { Button } from '@/components/ui/button'

import { search } from '../../lib/api'
import { Card, EmptyState } from '../../ui/controls'
import { FileList } from './FileList'
import { rowActions } from './RowMenu'
import { useNodeCommands } from './commands'

/**
 * Search results. `q` only — the filter chips and the size/date parameters the
 * API also accepts are cut for now, and half a filter UI is worse than none.
 *
 * The query lives in the URL, not in this component: the box in the chrome
 * navigates here, which makes a search a real location — shareable, in the
 * back button, and still there after a reload.
 *
 * The rows are the same rows a folder shows, with the same selection, band and
 * menus. What they cannot do is drag: a result has no list to be dragged within
 * and no trail to be dragged up.
 */
export function SearchPage() {
  const [params] = useSearchParams()
  const q = (params.get('q') ?? '').trim()

  const results = useQuery({
    queryKey: ['search', q],
    queryFn: () => search(q),
    enabled: q !== '',
  })

  // No parent folder to name: a result can come from anywhere, so a mutation
  // here re-reads every folder listing rather than one.
  const { handlers, commands, busy, dialogs } = useNodeCommands()

  const items = results.data?.items ?? []

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-4 py-6 sm:px-6 sm:py-8">
      <h1 className="text-[17px] font-semibold tracking-tight">
        {q === '' ? 'Search' : <>Results for “{q}”</>}
      </h1>

      {q === '' ? (
        <Card>
          <EmptyState
            icon={<Search className="size-5" />}
            title="Search across every folder"
            hint="Start typing in the box above — matching is on the name, anywhere in it."
          />
        </Card>
      ) : (
        <FileList
          nodes={items}
          pending={results.isPending}
          error={results.error}
          errorText="That search didn’t run. Try it again in a moment."
          onRetry={() => void results.refetch()}
          empty={
            <EmptyState
              icon={<Search className="size-5" />}
              title={`Nothing matches “${q}”.`}
              hint="Try a shorter piece of the name."
            />
          }
          selectable
          busy={busy}
          commands={commands}
          actions={(node) => rowActions(node, handlers)}
          rowExtra={(node) =>
            // A result is out of context by definition, so getting to the
            // folder it lives in is a real action here.
            node.parent_id && (
              <Button variant="ghost" size="icon-sm" aria-label={`Open the folder ${node.name} is in`} asChild>
                <Link to={`/folders/${node.parent_id}`} draggable={false}>
                  <FolderOpen />
                </Link>
              </Button>
            )
          }
        />
      )}

      {dialogs}
    </main>
  )
}
