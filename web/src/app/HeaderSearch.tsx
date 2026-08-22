import { useEffect, useState } from 'react'
import { useLocation, useNavigate, useSearchParams } from 'react-router'

import { SearchIcon } from '../ui/icons'

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

  // Leaving the results screen clears the box: a stale query sitting in the
  // chrome while a folder is on screen suggests the folder is a result.
  useEffect(() => {
    if (!onSearchScreen) setText('')
  }, [onSearchScreen])

  useEffect(() => {
    const trimmed = text.trim()
    if (trimmed === '' && !onSearchScreen) return
    const handle = setTimeout(() => {
      if (trimmed === '') {
        if (onSearchScreen) void navigate('/search', { replace: true })
        return
      }
      void navigate(`/search?q=${encodeURIComponent(trimmed)}`, { replace: onSearchScreen })
    }, 250)
    return () => clearTimeout(handle)
  }, [text, onSearchScreen, navigate])

  return (
    <label className="relative flex w-full items-center">
      <span className="sr-only">Search by name</span>
      <span aria-hidden className="pointer-events-none absolute left-2.5 text-ink-3">
        <SearchIcon />
      </span>
      <input
        type="search"
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="Search your files"
        className="w-full rounded-control border border-line-strong bg-surface py-1.5 pr-3 pl-8 text-[13px] text-ink outline-none transition duration-100 placeholder:text-ink-3 focus:border-teal focus:ring-2 focus:ring-teal/20"
      />
    </label>
  )
}
