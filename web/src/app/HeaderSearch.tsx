import { Search } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate, useSearchParams } from 'react-router'

/**
 * Search where a person expects it: in the chrome, on every screen, rather
 * than behind a destination they have to navigate to first.
 *
 * Typing navigates — the results screen owns the query, reading it from the
 * URL — so a search is a real location: shareable, bookmarkable, and still
 * there after a reload. The debounce lives on this side so a six-letter word
 * is one navigation, not six.
 */
export function HeaderSearch() {
  const [params] = useSearchParams()
  const location = useLocation()
  const navigate = useNavigate()
  const onSearchScreen = location.pathname === '/search'
  const [text, setText] = useState(onSearchScreen ? (params.get('q') ?? '') : '')

  // Read through a ref, never a dependency. The query string is what this
  // effect *writes*, so depending on it would make every navigation schedule
  // another one.
  const currentParams = useRef(params)
  currentParams.current = params

  // Leaving the results screen clears the box: a stale query sitting in the
  // chrome while a folder is on screen suggests the folder is a result.
  useEffect(() => {
    if (!onSearchScreen) setText('')
  }, [onSearchScreen])

  useEffect(() => {
    const trimmed = text.trim()
    if (trimmed === '' && !onSearchScreen) return
    const handle = setTimeout(() => {
      // Everything else riding on the URL — which file is open, how the list is
      // sorted — is state a person set deliberately, and a search must not be
      // what throws it away. Rebuilding the query string from `q` alone did.
      const next = new URLSearchParams(currentParams.current)
      if (trimmed === '') next.delete('q')
      else next.set('q', trimmed)
      const search = next.toString()
      void navigate({ pathname: '/search', search: search === '' ? '' : `?${search}` }, { replace: onSearchScreen })
    }, 250)
    return () => clearTimeout(handle)
  }, [text, onSearchScreen, navigate])

  return (
    <label className="relative flex w-full items-center">
      <span className="sr-only">Search by name</span>
      <span aria-hidden className="pointer-events-none absolute left-3.5 text-ink-3">
        <Search className="size-4" />
      </span>
      <input
        type="search"
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="Search your files"
        // No ring of its own: the one in `index.css` is the product's ring.
        // What is left is the field's own state — it lifts off the canvas onto
        // the surface as you type in it.
        className="h-10 w-full rounded-full border border-transparent bg-canvas pr-4 pl-10 text-[14px] text-ink transition duration-100 placeholder:text-ink-3 hover:border-line-strong focus:border-teal focus:bg-surface"
      />
    </label>
  )
}
