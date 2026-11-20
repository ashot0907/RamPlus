import type { ApiErrorBody, SessionInfo } from './types'

// Ошибка API: стабильный код (auth.required, plan.stale, op.busy, ...).
export class ApiError extends Error {
  constructor(
    public status: number,
    public body: ApiErrorBody,
  ) {
    super(body.message || body.code)
  }
  get code(): string {
    return this.body.code
  }
}

let csrf = ''

export function setCSRF(token: string | undefined): void {
  csrf = token ?? ''
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json'
    if (csrf) headers['X-CSRF-Token'] = csrf
  }
  let resp: Response
  try {
    resp = await fetch(path, {
      method,
      headers,
      body: method === 'GET' ? undefined : JSON.stringify(body ?? {}),
      credentials: 'same-origin',
    })
  } catch (e) {
    throw new ApiError(0, { code: 'network', message: String(e) })
  }
  if (resp.status === 204) return undefined as T
  let data: unknown = null
  try {
    data = await resp.json()
  } catch {
    data = { code: 'bad_response', message: `HTTP ${resp.status}` }
  }
  if (!resp.ok && resp.status !== 202) {
    throw new ApiError(resp.status, data as ApiErrorBody)
  }
  return data as T
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  session: () => request<SessionInfo>('GET', '/api/session'),
  login: (password: string) => request<SessionInfo>('POST', '/api/login', { password }),
  logout: () => request<void>('POST', '/api/logout', {}),
}

// SSE с переподключением: EventSource сам переподключается, onError
// сообщает о разрыве (демон недоступен или сессия истекла).
export function stream(
  path: string,
  handlers: Record<string, (data: unknown) => void>,
  onError?: () => void,
): () => void {
  const es = new EventSource(path, { withCredentials: true })
  for (const [name, fn] of Object.entries(handlers)) {
    es.addEventListener(name, (ev) => {
      try {
        fn(JSON.parse((ev as MessageEvent<string>).data))
      } catch {
        /* битое событие пропускаем */
      }
    })
  }
  es.onerror = () => onError?.()
  return () => es.close()
}
