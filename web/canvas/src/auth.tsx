// 认证态:一个 SPA、一处登录。任何 401 都收敛到 logout() → 回登录页。
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router'
import { api } from './api.ts'
import { session } from './session.ts'

interface AuthValue {
  user: string | null
  roles: string[]
  expiresAt: string | null
  canAuthor: boolean
  canApprove: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<string | null>(() => (session.authed ? session.user : null))
  const [roles, setRoles] = useState<string[]>(() => session.authed ? session.roles : [])
  const [expiresAt, setExpiresAt] = useState<string | null>(() => session.authed ? session.expiresAt : null)

  const login = useCallback(async (username: string, password: string) => {
    const res = await api.login(username, password)
    session.start(res.token, res.username, res.roles, res.expires_at)
    setUser(res.username)
    setRoles(res.roles)
    setExpiresAt(res.expires_at)
  }, [])

  const logout = useCallback(() => {
    session.clear()
    setUser(null)
    setRoles([])
    setExpiresAt(null)
  }, [])

  useEffect(() => {
    if (expiresAt === null) return
    const deadline = new Date(expiresAt).getTime()
    if (!Number.isFinite(deadline)) return
    let timer: number | undefined
    const schedule = () => {
      const remaining = deadline - Date.now()
      if (remaining <= 0) logout()
      else timer = window.setTimeout(schedule, Math.min(remaining, 2_147_483_647))
    }
    schedule()
    return () => window.clearTimeout(timer)
  }, [expiresAt, logout])

  // 这里只控制可见入口；后端继续执法角色、scope、租户与审批名单。
  // 旧会话无登录元数据时只读，不能从账号名或JWT猜测角色。
  const canAuthor = roles.some((r) => r === 'admin' || r === 'author')
  const canApprove = roles.some((r) => r === 'admin' || r === 'approver')
  const value = useMemo<AuthValue>(() => ({ user, roles, expiresAt, canAuthor, canApprove, login, logout }), [user, roles, expiresAt, canAuthor, canApprove, login, logout])
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthValue {
  const v = useContext(AuthContext)
  if (v === null) throw new Error('useAuth 必须在 AuthProvider 内使用')
  return v
}

/** 未登录一律回登录页;登录后回到原地址(带 from)。 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { user } = useAuth()
  const location = useLocation()
  if (user === null) {
    const from = `${location.pathname}${location.search}`
    return <Navigate to="/login" replace state={{ from }} />
  }
  return <>{children}</>
}
