// 认证态:一个 SPA、一处登录。任何 401 都收敛到 logout() → 回登录页。
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router'
import { api } from './api.ts'
import { session } from './session.ts'

interface AuthValue {
  user: string | null
  login: (username: string, password: string) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<string | null>(() => (session.authed ? session.user : null))

  const login = useCallback(async (username: string, password: string) => {
    const res = await api.login(username, password)
    session.start(res.token, username)
    setUser(username)
  }, [])

  const logout = useCallback(() => {
    session.clear()
    setUser(null)
  }, [])

  const value = useMemo<AuthValue>(() => ({ user, login, logout }), [user, login, logout])
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
