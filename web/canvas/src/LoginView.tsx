// 登录条:POST /api/v1/auth/login(authed 组外,同审批台端点);
// 401 统一 invalid_credentials 文案(防枚举同后端口径)——网络故障
// 不冒充凭证错误,否则把用户引向改密码死胡同。
import { useState } from 'react'
import { ApiError } from './api.ts'

export interface LoginCreds {
  username: string
  password: string
}

export function LoginView({ onLogin }: { onLogin: (creds: LoginCreds) => Promise<void> }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await onLogin({ username, password })
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError('登录失败:用户名或密码不正确')
      } else {
        setError('网络异常,请稍后重试')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="canvas-main">
      <form className="login-card" onSubmit={submit}>
        <h2>登录 braid</h2>
        <label className="f-row">
          <span className="f-label">用户名</span>
          <input className="f-text" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
        </label>
        <label className="f-row">
          <span className="f-label">密码</span>
          <input className="f-text" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        </label>
        <button type="submit" disabled={busy} data-testid="login-submit">
          {busy ? '登录中…' : '登录'}
        </button>
        {error !== '' ? (
          <p className="error" role="alert">
            {error}
          </p>
        ) : null}
      </form>
    </main>
  )
}
