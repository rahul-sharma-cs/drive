/**
 * REST client for everything except the upload protocol, which the engine owns
 * (`features/upload/engine/api.ts`) and which this file deliberately does not
 * duplicate.
 *
 * Two rules from the server contract shape it: every mutation carries
 * `X-Drive-Client` (GETs are exempt) — that header plus SameSite=Lax cookies is
 * the whole CSRF scheme, sound only because the API serves no cross-origin
 * credentialed CORS — and errors arrive as the
 * canonical `{code, message}` envelope, which becomes an `ApiError` so screens
 * can branch on the code and show the server's own message.
 */

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

const CLIENT_HEADER = { 'X-Drive-Client': 'web' }

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    credentials: 'same-origin',
    headers: body === undefined ? CLIENT_HEADER : { ...CLIENT_HEADER, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 204) return undefined as T
  const text = await res.text().catch(() => '')
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }
  if (!res.ok) {
    const env = (parsed ?? {}) as { code?: string; message?: string }
    throw new ApiError(res.status, env.code ?? 'internal', env.message ?? `${method} ${path} failed`)
  }
  return parsed as T
}

/* ------------------------------------------------------------------- types */

export interface Me {
  id: string
  email: string
  display_name: string
  root_id: string
  email_verified_at: string | null
}

export interface DriveNode {
  id: string
  parent_id: string | null
  kind: 'file' | 'folder'
  name: string
  size: number | null
  mime: string | null
  created_at: string
  updated_at: string
  trashed_root?: boolean
}

export interface Page<T> {
  items: T[]
  next_cursor: string | null
}

/** The upload status wire shape, as the UI needs it. */
export interface UploadSession {
  upload_id: string
  file_name: string
  file_size: number
  part_size: number
  parts_total: number
  parent_id: string | null
  status: 'active' | 'completing' | 'done' | 'aborted'
  confirmed_parts: number[]
  session_expires_at: string
}

/* ------------------------------------------------------------------- auth */

export const login = (email: string, password: string) =>
  request<Me>('POST', '/auth/login', { email, password })

export const signup = (email: string, password: string, displayName: string) =>
  request<{ status: string }>('POST', '/auth/signup', {
    email,
    password,
    display_name: displayName,
  })

export const verifyEmail = (token: string) => request<{ status: string }>('POST', '/auth/verify-email', { token })

export const logout = () => request<{ status: string }>('POST', '/auth/logout')

export const me = () => request<Me>('GET', '/auth/me')

/* ------------------------------------------------------------------ nodes */

export const getNode = (id: string) => request<DriveNode>('GET', `/nodes/${id}`)

export const listChildren = (id: string, cursor?: string) =>
  request<Page<DriveNode>>('GET', `/nodes/${id}/children${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`)

export const createFolder = (parentId: string, name: string) =>
  request<DriveNode & { existing?: boolean }>('POST', '/folders', { parent_id: parentId, name })

export const trashNode = (id: string) => request<void>('DELETE', `/nodes/${id}`)

/**
 * Rename and move are the same endpoint: a PATCH carrying whichever of the two
 * fields changed. `conflict_policy` is omitted on the first try so a collision
 * comes back as an error the person can answer, rather than being resolved
 * silently on their behalf.
 */
export const updateNode = (
  id: string,
  patch: { name?: string; parent_id?: string; conflict_policy?: 'rename' | 'replace' },
) => request<DriveNode>('PATCH', `/nodes/${id}`, patch)

/** Files only — the server answers 422 `unsupported` for a folder. */
export const copyNode = (id: string, parentId: string, conflictPolicy?: 'rename' | 'replace') =>
  request<DriveNode>('POST', `/nodes/${id}/copy`, { parent_id: parentId, conflict_policy: conflictPolicy })

export const restoreNode = (id: string) => request<DriveNode>('POST', `/nodes/${id}/restore`)

export const purgeNode = (id: string) => request<void>('DELETE', `/nodes/${id}/purge`)

export const listTrash = () => request<Page<DriveNode>>('GET', '/trash')

/** Quota and max_file_size are null when the deployment sets no cap at all. */
export interface Usage {
  used: number
  quota: number | null
  max_file_size: number | null
}

export const getUsage = () => request<Usage>('GET', '/usage')

export const search = (q: string) => request<Page<DriveNode>>('GET', `/search?q=${encodeURIComponent(q)}`)

export const listUploads = () => request<Page<UploadSession>>('GET', '/uploads')

export const discardUpload = (uploadId: string) => request<void>('DELETE', `/uploads/${uploadId}`)

/** Downloads are a top-level navigation to a 302 — never fetched into memory. */
export const downloadHref = (id: string) => `/api/files/${id}/download`
