import { Navigate, Route, Routes } from 'react-router'

import { AppLayout } from './app/AppLayout'
import { AccountPage } from './features/account/AccountPage'
import { LoginPage } from './features/auth/LoginPage'
import { RequireAuth } from './features/auth/RequireAuth'
import { SignupPage } from './features/auth/SignupPage'
import { VerifyPage } from './features/auth/VerifyPage'
import { FolderPage } from './features/browser/FolderPage'
import { SearchPage } from './features/browser/SearchPage'
import { TrashPage } from './features/browser/TrashPage'

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/signup" element={<SignupPage />} />
      <Route path="/verify" element={<VerifyPage />} />
      <Route element={<RequireAuth />}>
        <Route element={<AppLayout />}>
          <Route index element={<FolderPage />} />
          <Route path="/folders/:id" element={<FolderPage />} />
          <Route path="/trash" element={<TrashPage />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/account" element={<AccountPage />} />
        </Route>
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
