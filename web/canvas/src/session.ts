// 会话态:token 与用户名同存 sessionStorage(每标签页独立,与
// D-t44 #1 的 Bearer 载体拍板同源)。tokenStore 仍在 api.ts——它是
// HTTP 契约的一部分;这里只补"当前是谁"。
import { tokenStore } from './api.ts'

const USER_KEY = 'braid_canvas_user'
const ROLES_KEY = 'braid_canvas_roles'
const EXPIRY_KEY = 'braid_canvas_expires_at'

function readUser(): string | null {
  try {
    return sessionStorage.getItem(USER_KEY)
  } catch {
    return null
  }
}

export const session = {
  get user(): string | null {
    return readUser()
  },
  get authed(): boolean {
    return tokenStore.get() !== null
  },
  get roles(): string[] {
    try {
      const roles: unknown = JSON.parse(sessionStorage.getItem(ROLES_KEY) ?? '[]')
      return Array.isArray(roles) ? roles.filter((r): r is string => typeof r === 'string') : []
    } catch { return [] }
  },
  get expiresAt(): string | null {
    try { return sessionStorage.getItem(EXPIRY_KEY) } catch { return null }
  },
  start(token: string, username: string, roles: string[], expiresAt: string | null = null) {
    tokenStore.set(token)
    try {
      sessionStorage.setItem(USER_KEY, username)
      sessionStorage.setItem(ROLES_KEY, JSON.stringify(roles))
      if (expiresAt === null) sessionStorage.removeItem(EXPIRY_KEY)
      else sessionStorage.setItem(EXPIRY_KEY, expiresAt)
    } catch {
      /* 私密模式等存储不可用:仅本次内存态生效 */
    }
  },
  clear() {
    tokenStore.clear()
    try {
      sessionStorage.removeItem(USER_KEY)
      sessionStorage.removeItem(ROLES_KEY)
      sessionStorage.removeItem(EXPIRY_KEY)
    } catch {
      /* 同上 */
    }
  },
}
