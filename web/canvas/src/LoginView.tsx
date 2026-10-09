// 登录:整个产品只有这一处登录。单列、居中、一句话说清这是什么。
import { useState, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { TriangleAlert } from 'lucide-react'
import { ApiError } from './api.ts'
import { useAuth } from './auth.tsx'
import { BrandMark } from './shell/BrandMark.tsx'
import { Alert, Button } from './ui/primitives.tsx'
import { TextInput } from './ui/field.tsx'
import { TID } from './ui/testids.ts'

interface FromState {
  from?: string
}

export function LoginView() {
  const { login } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const from = (location.state as FromState | null)?.from ?? '/workflows'

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await login(username, password)
      void navigate(from, { replace: true })
    } catch (err) {
      setError(loginMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex h-full items-center justify-center bg-ground-0 px-6">
      <div className="w-full max-w-[22rem]">
        <div className="mb-7 flex flex-col items-start gap-2">
          <div className="flex items-center gap-2.5">
            <BrandMark size={26} />
            <span className="text-xl font-semibold tracking-[-0.03em] text-ink-0">braid</span>
          </div>
          <p className="text-sm text-ink-2">企业级 Agent 工作流编排平台</p>
        </div>

        <form className="flex flex-col gap-4" onSubmit={(e) => void submit(e)} data-testid={TID.loginForm}>
          <TextInput
            label="用户名"
            name="username"
            autoComplete="username"
            autoFocus
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            data-testid={TID.loginUsername}
          />
          <TextInput
            label="密码"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            data-testid={TID.loginPassword}
          />
          {error !== '' ? (
            <Alert tone="error" icon={TriangleAlert}>
              <span data-testid={TID.loginError}>{error}</span>
            </Alert>
          ) : null}
          <Button type="submit" variant="primary" disabled={busy} data-testid={TID.loginSubmit}>
            {busy ? '登录中…' : '登录'}
          </Button>
        </form>

        <p className="mt-8 flex items-center gap-2 text-micro text-ink-3">
          <span>私有化部署</span>
          <span aria-hidden>·</span>
          <span>全链路审计</span>
          <span aria-hidden>·</span>
          <span>工具调用隔离</span>
        </p>
      </div>
    </div>
  )
}

/** 401 与网络故障必须区分：后者不能把用户引向改密码的死胡同。 */
function loginMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'invalid_credentials':
      case 'unknown_user':
      case 'invalid_input':
        return '用户名或密码不正确。'
      case 'too_many_attempts':
        return '尝试次数过多，请稍后再试。'
      case 'account_locked':
        return '账号已被锁定，请联系管理员。'
      case 'user_disabled':
        return '账号已停用。'
      default:
        return `登录失败（${err.code}）。`
    }
  }
  return '无法连接服务，请检查网络后重试。'
}
