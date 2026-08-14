/**
 * Network layer: the upload REST endpoints (PLAN's frozen Appendix) and the
 * part-PUT transport.
 *
 * Part PUTs go over XMLHttpRequest, not fetch: fetch has no upload-progress
 * events, and the engine needs `xhr.upload.onprogress` (stall watchdog),
 * `getResponseHeader("ETag")` and `xhr.abort()` (pause/cancel).
 */

import {
  ApiError,
  NetworkError,
  type CompleteResponse,
  type ConfirmResponse,
  type CreateRequest,
  type CreateResponse,
  type NodeSummary,
  type PartPutter,
  type PutOutcome,
  type ResumeResponse,
  type SessionStatus,
  type UploadApi,
} from './types'

export type FetchLike = (input: string, init: RequestInit) => Promise<Response>

/** Every `/api` mutation carries this header (PLAN §CSRF). */
const CLIENT_HEADER = { 'X-Drive-Client': 'web' }

export class HttpUploadApi implements UploadApi {
  constructor(
    private readonly fetchFn: FetchLike = (input, init) => fetch(input, init),
    private readonly base = '/api',
  ) {}

  create(req: CreateRequest): Promise<CreateResponse> {
    return this.send('POST', '/uploads', req)
  }

  status(uploadId: string): Promise<SessionStatus> {
    return this.send('GET', `/uploads/${encodeURIComponent(uploadId)}`)
  }

  resume(uploadId: string, partMd5s?: Record<string, string>): Promise<ResumeResponse> {
    return this.send('POST', `/uploads/${encodeURIComponent(uploadId)}/resume`, partMd5s ? { part_md5s: partMd5s } : {})
  }

  confirm(
    uploadId: string,
    partNumber: number,
    body: { etag: string; md5: string; size: number },
  ): Promise<ConfirmResponse> {
    return this.send('POST', `/uploads/${encodeURIComponent(uploadId)}/parts/${partNumber}`, body)
  }

  complete(uploadId: string, sha256: string): Promise<CompleteResponse> {
    return this.send('POST', `/uploads/${encodeURIComponent(uploadId)}/complete`, { sha256 })
  }

  async remove(uploadId: string): Promise<void> {
    await this.send('DELETE', `/uploads/${encodeURIComponent(uploadId)}`)
  }

  node(nodeId: string): Promise<NodeSummary> {
    return this.send('GET', `/nodes/${encodeURIComponent(nodeId)}`)
  }

  private async send<T>(method: string, path: string, body?: unknown): Promise<T> {
    let res: Response
    try {
      res = await this.fetchFn(this.base + path, {
        method,
        credentials: 'same-origin',
        headers: body === undefined ? CLIENT_HEADER : { ...CLIENT_HEADER, 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    } catch (e) {
      throw new NetworkError(`${method} ${path}: ${(e as Error)?.message ?? 'request failed'}`)
    }
    // 5xx is indistinguishable from "backend down" for the engine's purposes.
    if (res.status >= 500) throw new NetworkError(`${method} ${path}: ${res.status}`)
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
      throw new ApiError(res.status, env.code ?? 'invalid', env.message ?? `${method} ${path} failed`)
    }
    return parsed as T
  }
}

/** XHR part PUT. Non-2xx *resolves* — the engine classifies it. */
export const xhrPutPart: PartPutter = (url, body, onProgress) => {
  const xhr = new XMLHttpRequest()
  let aborted = false
  const promise = new Promise<PutOutcome>((resolve) => {
    xhr.open('PUT', url, true)
    xhr.upload.onprogress = (e: ProgressEvent) => onProgress(e.loaded)
    xhr.onload = () =>
      resolve({
        kind: 'response',
        status: xhr.status,
        etag: xhr.getResponseHeader('ETag'),
        body: xhr.responseText ?? '',
      })
    xhr.onerror = () => resolve({ kind: 'network', message: 'part PUT failed' })
    xhr.ontimeout = () => resolve({ kind: 'network', message: 'part PUT timed out' })
    xhr.onabort = () => resolve({ kind: 'aborted' })
    // Content-Type is deliberately not set: objects must be stored with none
    // (PLAN §Serving user content — Range GETs skip response-content-* overrides).
    xhr.send(body)
  })
  return {
    promise,
    abort() {
      if (aborted) return
      aborted = true
      xhr.abort()
    },
  }
}
