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
      <BrowserRouter>
        <App />
        {/* Top-right: the upload manager occupies the bottom-right corner. */}
        <Toaster
          position="top-right"
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
