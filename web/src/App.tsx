import { Navigate, Route, Routes } from 'react-router'

import { AppLayout } from './app/AppLayout'
import { LoginPage } from './features/auth/LoginPage'
import { RequireAuth } from './features/auth/RequireAuth'
import { SignupPage } from './features/auth/SignupPage'
import { VerifyPage } from './features/auth/VerifyPage'

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/signup" element={<SignupPage />} />
      <Route path="/verify" element={<VerifyPage />} />
      <Route element={<RequireAuth />}>
        <Route element={<AppLayout />}>
          <Route index element={<p className="p-6 text-sm text-neutral-500">Your files will appear here.</p>} />
        </Route>
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
