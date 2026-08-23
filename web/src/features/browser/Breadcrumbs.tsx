import { Fragment, useState } from 'react'
import { Link, useNavigate } from 'react-router'

import type { DriveNode } from '../../lib/api'
import { isPlainClick, navigateWithTransition } from '../../lib/viewTransition'
import { dragPayload, isNodeDrag } from './dnd'

/**
 * The trail from the root down to the folder on screen — and the only way to
 * drag something "up", since an ancestor is nowhere on the list to drop onto.
 *
 * It stays above the command band and keeps its drop handlers while rows are
 * selected: moving a selection to a parent is exactly when this is wanted.
 */
export function Breadcrumbs({
  crumbs,
  rootId,
  onDropInto,
}: {
  crumbs: DriveNode[]
  rootId: string
  /** The dragged ids were dropped on an ancestor. */
  onDropInto: (id: string, ids: string[]) => void
}) {
  const [dropTarget, setDropTarget] = useState<string | null>(null)
  const navigate = useNavigate()

  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 flex-wrap items-center gap-1.5 text-sm text-ink-3">
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1
        const label = crumb.id === rootId ? 'My Drive' : crumb.name
        return (
          <Fragment key={crumb.id}>
            {i > 0 && (
              <span aria-hidden className="text-line-strong">
                /
              </span>
            )}
            {last ? (
              // The trail doubles as the page title: the folder you are in is
              // the largest thing on the screen after the files themselves.
              <span aria-current="page" className="truncate text-[17px] font-semibold tracking-tight text-ink">
                {label}
              </span>
            ) : (
              <Link
                className={`truncate rounded px-1 transition duration-100 hover:text-ink ${
                  dropTarget === crumb.id ? 'bg-teal-soft text-teal-strong ring-1 ring-teal' : ''
                }`}
                to={crumb.id === rootId ? '/' : `/folders/${crumb.id}`}
                // Going up is folder navigation too, and crossfades the same way.
                onClick={(e) => {
                  if (!isPlainClick(e)) return
                  e.preventDefault()
                  navigateWithTransition(() => void navigate(crumb.id === rootId ? '/' : `/folders/${crumb.id}`))
                }}
                onDragOver={(e) => {
                  if (!isNodeDrag(e.dataTransfer)) return
                  e.preventDefault()
                  setDropTarget(crumb.id)
                }}
                onDragLeave={() => setDropTarget(null)}
                onDrop={(e) => {
                  if (!isNodeDrag(e.dataTransfer)) return
                  e.preventDefault()
                  setDropTarget(null)
                  // The ids come off the drag itself. Reading the selection
                  // instead would move whatever happened to be selected when
                  // the drag started somewhere else.
                  const ids = dragPayload(e.dataTransfer)
                  if (ids) onDropInto(crumb.id, ids)
                }}
              >
                {label}
              </Link>
            )}
          </Fragment>
        )
      })}
    </nav>
  )
}
