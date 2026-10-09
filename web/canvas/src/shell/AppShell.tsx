// 壳:一条顶栏承载三件事——身份、域导航、系统脉冲。
// 脉冲条是色相在壳层里唯一的工作:执行中 / 等待审批 / 失败 三个计数,
// 其中"等待审批"是 braid 的核心运营数字(挂起不占 worker,但人在等)。
import { useEffect, useState, type ReactNode } from 'react'
import { NavLink } from 'react-router'
import { Activity, LogOut, Workflow } from 'lucide-react'
import { ApiError, api } from '../api.ts'
import { useAuth } from '../auth.tsx'
import { I } from '../ui/I.tsx'
import { TID } from '../ui/testids.ts'
import { Wordmark } from './BrandMark.tsx'

interface Pulse {
  running: number
  waiting: number
  failed: number
}

function usePulse(active: boolean): Pulse | null {
  const [pulse, setPulse] = useState<Pulse | null>(null)
  useEffect(() => {
    if (!active) return
    let alive = true
    let timer: number | undefined

    const tick = async () => {
      try {
        const [run, wait, fail] = await Promise.all([
          api.listExecutions({ status: 'RUNNING', limit: 1 }),
          api.listExecutions({ status: 'WAITING_APPROVAL', limit: 1 }),
          api.listExecutions({ status: 'FAILED', limit: 1 }),
        ])
        if (!alive) return
        setPulse({ running: run.page.total, waiting: wait.page.total, failed: fail.page.total })
      } catch (err) {
        // 脉冲是增益不是依赖:401 由调用方登出,其余静默降级。
        if (err instanceof ApiError && err.status === 401) return
        if (alive) setPulse(null)
      }
    }

    void tick()
    timer = window.setInterval(() => void tick(), 20_000)
    return () => {
      alive = false
      if (timer !== undefined) window.clearInterval(timer)
    }
  }, [active])
  return pulse
}

function PulseStat({ tone, value, label }: { tone: string; value: number; label: string }) {
  return (
    <span className="flex items-baseline gap-1.5" title={`${label} ${value}`}>
      <span
        className="inline-block size-1.5 translate-y-[-1px] rounded-full"
        style={{ background: `var(--color-sig-${tone})` }}
        aria-hidden
      />
      <span className="num text-sm font-semibold text-ink-0">{value}</span>
      <span className="text-xs text-ink-2">{label}</span>
    </span>
  )
}

const navClass = 'tab'

export function AppShell({ children }: { children: ReactNode }) {
  const { user, logout } = useAuth()
  const pulse = usePulse(user !== null)

  return (
    <div className="flex h-full flex-col bg-ground-0" data-testid={TID.shell}>
      <header className="flex h-13 shrink-0 items-center gap-6 border-b border-line-1 bg-ground-1 px-4">
        <Wordmark />

        <nav className="tabs" aria-label="主域导航">
          <NavLink to="/workflows" className={navClass} data-testid={TID.navWorkflows}>
            <I glyph={Workflow} />
            工作流
          </NavLink>
          <NavLink to="/executions" className={navClass} data-testid={TID.navExecutions}>
            <I glyph={Activity} />
            执行
          </NavLink>
        </nav>

        <div className="ml-auto flex items-center gap-5">
          {pulse !== null ? (
            <div className="hidden items-center gap-4 border-r border-line-0 pr-5 xl:flex" aria-label="运行脉冲">
              <PulseStat tone="run" value={pulse.running} label="执行中" />
              <PulseStat tone="wait" value={pulse.waiting} label="待审批" />
              <PulseStat tone="fail" value={pulse.failed} label="失败" />
            </div>
          ) : null}

          <div className="flex items-center gap-2">
            <span className="mono text-xs text-ink-1" data-testid={TID.whoami} title="当前账号">
              {user}
            </span>
            <button type="button" className="btn btn-quiet btn-sm btn-icon" onClick={logout} title="退出登录" data-testid={TID.logout}>
              <I glyph={LogOut} size={13} />
              <span className="sr-only">退出登录</span>
            </button>
          </div>
        </div>
      </header>

      <main className="flex min-h-0 flex-1 flex-col">{children}</main>
    </div>
  )
}

/* 路由页头:标题 + 说明 + 动作。纵向留白比下方多(读起来属于下一块)。 */
export function RouteHeader({
  title,
  sub,
  actions,
}: {
  title: string
  sub?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="flex h-14 shrink-0 items-center gap-4 border-b border-line-0 px-4">
      <h1 className="text-lg font-semibold tracking-[-0.02em] text-ink-0">{title}</h1>
      {sub !== undefined ? <div className="min-w-0 text-xs text-ink-2 num">{sub}</div> : null}
      <div className="ml-auto flex items-center gap-2">{actions}</div>
    </div>
  )
}
