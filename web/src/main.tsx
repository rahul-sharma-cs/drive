import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import { Toaster } from 'sonner'

import App from './App'
import './index.css'

// No automatic retries and no refetch on focus. Both defaults are wrong here:
// the API answers failures with a message worth showing immediately rather than
// silently three times over, and a refocused tab refetching every list on the
// screen is a burst of requests for answers this app's own mutations already
// keep current.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* `useTransitions={false}`, and the folder crossfade is why.
          `navigateWithTransition` hands `startViewTransition` a callback that
          has to leave the new screen in the DOM by the time it returns — that
          is what the browser snapshots. Left at the default, the router marks
          every location change as a React transition, which is exactly the
          kind of update `flushSync` does not flush: the callback returns with
          the old rows still on the page, the browser animates one state
          against itself, and the new folder appears afterwards with no
          crossfade at all. Nothing about it fails loudly, which is why it is
          spelled out here.

          The cost is the thing transitions buy — a slow route staying
          interactive on the old screen instead of suspending — and every
          screen in this app is already mounted synchronously off cached
          queries, so there is nothing to keep interactive. */}
      <BrowserRouter useTransitions={false}>
        <App />
        {/* Top-right: the upload manager occupies the bottom-right corner. */}
        <Toaster
          position="top-right"
          // Clear of the 56px bar rather than on top of it: the default offset
          // puts the first toast over the account avatar.
          offset={{ top: '4.5rem', right: '1rem' }}
          mobileOffset={{ top: '4.25rem', left: '0.75rem', right: '0.75rem' }}
          theme="light"
          toastOptions={{
            classNames: {
              toast: 'rounded-card border border-line bg-surface text-ink shadow-dock',
              title: 'text-[13px] font-medium',
              description: 'text-[12px] text-ink-3',
            },
          }}
        />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
