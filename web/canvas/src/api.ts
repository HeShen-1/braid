// API 客户端:同源 /api/v1,Bearer + sessionStorage(载体与审批台同
// 拍板,D-t44 #1;sessionStorage 每标签页独立,画布自带登录条)。
// 401 统一抛 ApiError(status=401),App 层清 token 回登录。
import type { WorkflowDoc } from './model/dsl.ts'

const TOKEN_KEY = 'braid_canvas_token'

export interface Issue {
  code: string
  node?: string
  ref?: string
  detail: string
}

export class ApiError extends Error {
  status: number
  code: string
  issues: Issue[]

  constructor(status: number, code: string, issues: Issue[]) {
    super(`api ${status} ${code}`)
    this.status = status
    this.code = code
    this.issues = issues
  }
}

export const tokenStore = {
  get(): string | null {
    try {
      return sessionStorage.getItem(TOKEN_KEY)
    } catch {
      return null
    }
  },
  set(token: string) {
    try {
      sessionStorage.setItem(TOKEN_KEY, token)
    } catch {
      /* 私密模式等存储不可用:仅本次内存态生效 */
    }
  },
  clear() {
    try {
      sessionStorage.removeItem(TOKEN_KEY)
    } catch {
      /* 同上 */
    }
  },
}

async function call<T>(method: string, path: string, body?: string): Promise<T> {
  const headers: Record<string, string> = {}
  const token = tokenStore.get()
  if (token !== null) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, { method, headers, body })
  if (!res.ok) {
    let code = `http_${res.status}`
    let issues: Issue[] = []
    try {
      const payload = (await res.json()) as { error?: string; issues?: Issue[] }
      if (payload.error !== undefined) code = payload.error
      if (Array.isArray(payload.issues)) issues = payload.issues
    } catch {
      /* 非 JSON 错误体:保底 code */
    }
    throw new ApiError(res.status, code, issues)
  }
  if (res.status === 204) return undefined as T
  const text = await res.text()
  return (text === '' ? undefined : JSON.parse(text)) as T
}

// ---- 契约面(internal/api definitionDTO / login 响应) ----

export interface DraftSummary {
  id: string
  name: string
  version: number | null
  status: string
  description: string
  updated_at: string
}

export interface DraftDetail extends DraftSummary {
  dsl: WorkflowDoc
}

export interface LoginResponse {
  token: string
  token_type: string
  expires_at: string
}

// 端点全字面量(不拼前缀):bundle 端点锚与 grep 语义都锚在完整路径上
// (web_test canvas bundle 锚同 app.js 纪律)。
export const api = {
  login(username: string, password: string): Promise<LoginResponse> {
    return call('POST', '/api/v1/auth/login', JSON.stringify({ username, password }))
  },
  listDrafts(): Promise<{ items: DraftSummary[] }> {
    return call('GET', '/api/v1/definitions?status=DRAFT')
  },
  getDraft(id: string): Promise<DraftDetail> {
    return call('GET', `/api/v1/drafts/${id}`)
  },
  createDraft(serialized: string): Promise<DraftDetail> {
    return call('POST', '/api/v1/definitions', serialized)
  },
  updateDraft(id: string, serialized: string): Promise<DraftDetail> {
    return call('PUT', `/api/v1/drafts/${id}`, serialized)
  },
  publishDraft(id: string): Promise<DraftDetail> {
    return call('POST', `/api/v1/drafts/${id}/publish`)
  },
}
