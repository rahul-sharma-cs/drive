import { lazy, Suspense } from 'react'
import { Navigate, Route, Routes } from 'react-router'

import { AppLayout } from './app/AppLayout'
import { AccountPage } from './features/account/AccountPage'
import { ForgotPage } from './features/auth/ForgotPage'
import { LoginPage } from './features/auth/LoginPage'
import { ResetPage } from './features/auth/ResetPage'
import { RequireAuth } from './features/auth/RequireAuth'
import { SignupPage } from './features/auth/SignupPage'
import { VerifyPage } from './features/auth/VerifyPage'
import { FolderPage } from './features/browser/FolderPage'
import { SearchPage } from './features/browser/SearchPage'
import { TrashPage } from './features/browser/TrashPage'
import { SharedLinksPage } from './features/share/SharedLinksPage'

/**
 * A chunk of its own, like the viewer: the share page is the one screen an
 * owner's session never visits, and a recipient's visit never needs the rest.
 */
const SharePage = lazy(() => import('./features/share/SharePage'))

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/signup" element={<SignupPage />} />
      <Route path="/verify" element={<VerifyPage />} />
      {/* Both reset screens are reached from a mail link by someone who cannot
          sign in, so they sit outside RequireAuth beside /login. */}
      <Route path="/forgot" element={<ForgotPage />} />
      <Route path="/reset" element={<ResetPage />} />
      {/* A recipient: no session, no chrome, no `/api/auth/me`. Explicit,
          because the catch-all below would otherwise send them to /login. */}
      <Route
        path="/s/:token"
        element={
          <Suspense fallback={<p className="p-6 text-sm text-ink-3">Loading…</p>}>
            <SharePage />
          </Suspense>
        }
      />
      <Route element={<RequireAuth />}>
        <Route element={<AppLayout />}>
          <Route index element={<FolderPage />} />
          <Route path="/folders/:id" element={<FolderPage />} />
          <Route path="/shared" element={<SharedLinksPage />} />
          <Route path="/trash" element={<TrashPage />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/account" element={<AccountPage />} />
        </Route>
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
