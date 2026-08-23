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
  /**
   * Whether this account can be signed in to with a password at all. False for
   * an account whose only sign-in method is a linked identity — including one
   * whose password was cleared when a Google sign-in activated it.
   */
  has_password: boolean
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
  /** Trash listings only — when this node was thrown away. */
  deleted_at?: string
  /** Folders in a children listing only — how many live things are inside. */
  item_count?: number
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

export const updateMe = (displayName: string) => request<Me>('PATCH', '/auth/me', { display_name: displayName })

/**
 * Succeeds with 204 and revokes every session but this one, so the answer is
 * "you are still signed in here, and nowhere else". A wrong current password is
 * a 401 carrying the server's own wording.
 */
export const changePassword = (currentPassword: string, newPassword: string) =>
  request<void>('POST', '/auth/password', { current_password: currentPassword, new_password: newPassword })

/** One live sign-in. `current` marks the session this browser is using. */
export interface AuthSession {
  id: string
  created_at: string
  last_seen_at: string | null
  ip: string | null
  user_agent: string | null
  current: boolean
}

export const listSessions = () => request<Page<AuthSession>>('GET', '/auth/sessions')

export const revokeSession = (id: string) => request<void>('DELETE', `/auth/sessions/${id}`)

export const logoutAll = () => request<void>('POST', '/auth/logout-all')

/**
 * Both of these answer 200 whether or not the address has an account — the
 * server refuses to be an account-existence oracle, and the screens that call
 * them must not become one either.
 */
export const requestReset = (email: string) => request<{ status: string }>('POST', '/auth/password-reset', { email })

export const resendVerification = (email: string) =>
  request<{ status: string }>('POST', '/auth/resend-verification', { email })

export const confirmReset = (token: string, newPassword: string) =>
  request<void>('POST', '/auth/password-reset/confirm', { token, new_password: newPassword })

/* ------------------------------------------------- other ways in */

/**
 * Which third-party sign-ins this deployment is configured for. Answered
 * without a session and outside the auth bucket, because the signed-out screens
 * have to ask it before anyone has proved anything.
 */
export interface Providers {
  google: boolean
}

export const getProviders = () => request<Providers>('GET', '/auth/providers')

/** One linked sign-in method. The provider's subject is never sent to us. */
export interface Identity {
  id: string
  provider: 'google'
  /** The address the provider asserted when the link was made. */
  email_at_link: string
  created_at: string
  last_login_at: string | null
}

export const listIdentities = () => request<Page<Identity>>('GET', '/auth/identities')

/**
 * 204 when it is gone, 404 when it never was yours (unknown and not-yours are
 * deliberately the same answer), and 409 `unsupported` when it is the account's
 * last way in — that last one carries the server's own wording.
 */
export const unlinkIdentity = (id: string) => request<void>('DELETE', `/auth/identities/${id}`)

/* ------------------------------------------------------------------ nodes */

export const getNode = (id: string) => request<DriveNode>('GET', `/nodes/${id}`)

/**
 * One page of a folder. `sort` is sent only when the caller wants something
 * other than the server's own default of name ascending — see `useChildren`,
 * which is the one place that decides. A cursor is only valid under the sort it
 * was issued for; the server answers 422 if the two disagree.
 */
export const listChildren = (id: string, cursor?: string, sort?: Sort) => {
  const query: string[] = []
  if (sort) query.push(`sort=${sort.key}`, `dir=${sort.dir}`)
  if (cursor) query.push(`cursor=${encodeURIComponent(cursor)}`)
  return request<Page<DriveNode>>(
    'GET',
    `/nodes/${id}/children${query.length > 0 ? `?${query.join('&')}` : ''}`,
  )
}

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

/**
 * One page of the trash, newest deletion first. Paged like the folder listing
 * and for the same reason: a trash nobody has emptied in a while is long, and
 * a screen that shows only what fits in one answer lets a select-all quietly
 * mean "some".
 */
export const listTrash = (cursor?: string) =>
  request<Page<DriveNode>>('GET', `/trash${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`)

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

/**
 * A presigned link to one file's bytes, and the moment it stops working.
 *
 * `url` points at the object store, not at this app. Hand it straight to an
 * `<img>`/`<video>`/`<audio>`/`<iframe>`, or fetch it with a bare `fetch(url)` —
 * never through `request()`, whose `X-Drive-Client` header would turn a plain
 * cross-origin GET into a preflight the store's CORS rule answers with a 403.
 */
export interface BlobLink {
  /** Presigned, short-lived, and good for exactly one file. */
  url: string
  /** RFC 3339. Callers that hold a link open re-ask before this passes. */
  expires_at: string
}

/**
 * `mime` is the server's own normalized constant for the type it agreed to
 * serve inline — not the stored, client-declared one. Text-like types all
 * arrive as `text/plain`; a type it will not serve inline never gets a URL.
 */
export interface PreviewLink extends BlobLink {
  mime: string
}

/** 415 `unsupported` for anything the server will not serve inline (SVG, HTML). */
export const getPreview = (id: string) => request<PreviewLink>('GET', `/files/${id}/preview`)

/** The same link the download route answers with a 302, as JSON. */
export const getDownloadLink = (id: string) =>
  request<BlobLink>('GET', `/files/${id}/download?format=json`)

/* --------------------------------------------------------- sorting a folder */

export type SortKey = 'name' | 'updated_at' | 'size'
export type SortDir = 'asc' | 'desc'
export interface Sort {
  key: SortKey
  dir: SortDir
}

/** What the server does when asked for nothing. Folders come first either way. */
export const DEFAULT_SORT: Sort = { key: 'name', dir: 'asc' }

export const isDefaultSort = (sort: Sort) => sort.key === DEFAULT_SORT.key && sort.dir === DEFAULT_SORT.dir

/* ------------------------------------------------------- the trash, in bulk */

/**
 * What one id in a bulk trash call came to.
 *
 * `pending` is the interesting one: the routes share a wall-clock budget and
 * stop when it runs out, so a long list comes back part-done and the caller
 * sends the rest. `not_found` is not a failure — it means the row is already
 * gone, which is what was asked for.
 */
export type BulkStatus = 'ok' | 'not_found' | 'name_conflict' | 'pending' | 'error'

export interface BulkResult {
  id: string
  status: BulkStatus
}

export interface BulkAnswer {
  results: BulkResult[]
  /** There is still work left — call again. */
  remaining: boolean
}

/** The server refuses more than this per call. `runBulk` in `queries.ts` chunks to it. */
export const BULK_LIMIT = 200

export const restoreNodes = (ids: string[]) => request<BulkAnswer>('POST', '/trash/restore', { ids })

export const purgeNodes = (ids: string[]) => request<BulkAnswer>('POST', '/trash/purge', { ids })

/**
 * Empties the whole trash, a page of roots at a time — not just what is loaded
 * on screen. `remaining` says another call is needed.
 */
export const emptyTrash = () => request<{ purged: number; remaining: boolean }>('DELETE', '/trash')

/* ----------------------------------------------------------------- shares */

/**
 * What a share knows about its file. `parent_id` is what the shared-links
 * list builds its row link from; a file always has one, but the column is
 * nullable on the wire and the type says so.
 */
export interface ShareNode {
  id: string
  parent_id: string | null
  name: string
  size: number | null
  mime: string | null
}

/**
 * One active link, as the owner sees it. Never the token and never its hash:
 * the URL is handed out exactly once, by `createShare` and `regenerateShare`,
 * and the server cannot show it again.
 */
export interface Share {
  id: string
  node: ShareNode
  /** False while the file sits in the trash — the link is inert until a restore. */
  node_live: boolean
  has_password: boolean
  expires_at: string | null
  max_downloads: number | null
  download_count: number
  created_at: string
}

/**
 * What a link is created with. All three, `null` for "none": a create has no
 * current state to keep, so there is nothing tri-state about it.
 */
export interface ShareSettings {
  expires_at: string | null
  password: string | null
  max_downloads: number | null
}

/**
 * What a settings PATCH carries. `expires_at` and `max_downloads` are always
 * sent (`null` clears / means unlimited); `password` goes on the wire only
 * when the person acted on it — **absent** keeps the current password, `null`
 * takes it off, a string sets a new one. That is what lets an expiry change
 * leave a password standing without anyone re-typing it.
 */
export interface ShareSettingsPatch {
  expires_at: string | null
  max_downloads: number | null
  password?: string | null
}

/** What a recipient is told before anything is minted. Read without a session. */
export interface ShareMeta {
  name: string
  size: number | null
  mime: string | null
  requires_password: boolean
  expires_at: string | null
  /** The cap is spent, and this browser is not the one that spent it. */
  exhausted: boolean
  /** The server will sign an inline link for this type — images, video, audio, text. */
  preview: boolean
}

/** A share with the one copy of its URL anyone will ever be given. */
export interface MintedShare {
  share: Share
  url: string
}

/**
 * 201 with the URL, 409 `exists` when the file already has an active link,
 * 422 for a folder, a past expiry, a short password or a cap out of range.
 */
export const createShare = (nodeId: string, settings: ShareSettings) =>
  request<MintedShare>('POST', '/shares', { node_id: nodeId, ...settings })

/** The caller's active shares, newest first. `nodeId` narrows it to one file. */
export const listShares = (cursor?: string, nodeId?: string) => {
  const query: string[] = []
  if (nodeId) query.push(`node_id=${encodeURIComponent(nodeId)}`)
  if (cursor) query.push(`cursor=${encodeURIComponent(cursor)}`)
  return request<Page<Share>>('GET', `/shares${query.length > 0 ? `?${query.join('&')}` : ''}`)
}

/** A new token for the same file: the old URL stops, the count starts at zero. */
export const regenerateShare = (id: string) =>
  request<MintedShare>('PATCH', `/shares/${id}`, { action: 'regenerate' })

/**
 * The answer's shape is not relied on: every caller re-reads the share
 * through the `['shares']` invalidation rather than trusting what came back.
 * An absent `password` key survives to the wire — `JSON.stringify` drops it —
 * which is the "keep" of the tri-state.
 */
export const updateShareSettings = (id: string, settings: ShareSettingsPatch) =>
  request<unknown>('PATCH', `/shares/${id}`, settings)

/** 204. The row stays, for the access log; the link stops. */
export const revokeShare = (id: string) => request<void>('DELETE', `/shares/${id}`)

const sharePath = (token: string) => `/s/${encodeURIComponent(token)}`

/** One 404 for unknown, revoked, expired and trashed alike — deliberately. */
export const getShareMeta = (token: string) => request<ShareMeta>('GET', `${sharePath(token)}/meta`)

/** Passwordless shares only. Idempotent per browser: a second call extends the session it has. */
export const openShareSession = (token: string) => request<void>('POST', `${sharePath(token)}/session`)

/** 401 for a wrong password, 429 when this address has guessed too often. */
export const openShareWithPassword = (token: string, password: string) =>
  request<void>('POST', `${sharePath(token)}/password`, { password })

/** Needs a guest session (401 without one); 415 for a type not shown to strangers, PDF included. */
export const getSharePreview = (token: string) => request<PreviewLink>('GET', `${sharePath(token)}/preview`)

/**
 * A navigation, like `downloadHref`: the server answers a 302 to the bytes, or
 * a 302 back to the share page with `?reason=` when it refuses.
 */
export const shareDownloadHref = (token: string) => `/api${sharePath(token)}/download`
