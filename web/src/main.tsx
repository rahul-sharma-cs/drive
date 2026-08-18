import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import { Toaster } from 'sonner'

import App from './App'
import './index.css'

// No automatic retries and no refetch on focus. Both defaults are wrong here:
// the API answers failures with a message worth showing immediately, and the
// whole /api/auth surface sits behind a 10/min per-IP bucket that a refocusing
// tab would spend for nothing.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
        <Toaster position="bottom-right" />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
