// 壳:一条顶栏承载三件事——身份、域导航、系统脉冲。
// 脉冲条是色相在壳层里唯一的工作:执行中 / 等待审批 / 失败 三个计数,
// 其中"等待审批"是 braid 的核心运营数字(挂起不占 worker,但人在等)。
import { useEffect, useState, type ReactNode } from 'react'
import { NavLink } from 'react-router'
import { Activity, LogOut, Stamp, Workflow } from 'lucide-react'
import { ApiError, api } from '../api.ts'
import { useAuth } from '../auth.tsx'
import { I } from '../ui/I.tsx'
import { TID } from '../ui/testids.ts'
import { Wordmark } from './BrandMark.tsx'
import { ThemeControl } from './ThemeControl.tsx'
import { ConfirmLayer, useConfirmLayer } from '../ui/confirm.tsx'
import { useWorkflow } from '../workflow/WorkflowProvider.tsx'
import { CONSOLE_REFRESH, LIVE_REFRESH_MS } from '../refresh.ts'
import { apiErrorText } from '../ui/errors.ts'

interface Pulse {
  running: number
  waiting: number
  failed: number
}

function usePulse(active: boolean, onAuthExpired: () => void): { pulse: Pulse | null; error: string } {
  const [pulse, setPulse] = useState<Pulse | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    if (!active) return
    let alive = true
    let seq = 0

    const tick = async () => {
      const request = ++seq
      const results = await Promise.allSettled([
          api.listExecutions({ status: 'RUNNING', limit: 1 }),
          api.listExecutions({ status: 'WAITING_APPROVAL', limit: 1 }),
          api.listExecutions({ status: 'FAILED', limit: 1 }),
      ])
      if (!alive || request !== seq) return
      for (const result of results) {
        if (result.status === 'rejected' && result.reason instanceof ApiError && result.reason.status === 401) {
          onAuthExpired()
          return
        }
      }
      const [run, wait, fail] = results
      if (run.status === 'fulfilled' && wait.status === 'fulfilled' && fail.status === 'fulfilled') {
        setPulse({ running: run.value.page.total, waiting: wait.value.page.total, failed: fail.value.page.total })
        setError('')
      } else {
        const failed = results.find((r) => r.status === 'rejected')
        setError(failed?.status === 'rejected' ? apiErrorText(failed.reason) : '运行脉冲未能读取。')
      }
    }

    void tick()
    const refresh = () => { void tick() }
    const timer = window.setInterval(refresh, LIVE_REFRESH_MS)
    window.addEventListener(CONSOLE_REFRESH, refresh)
    window.addEventListener('focus', refresh)
    return () => {
      alive = false
      window.clearInterval(timer)
      window.removeEventListener(CONSOLE_REFRESH, refresh)
      window.removeEventListener('focus', refresh)
    }
  }, [active, onAuthExpired])
  return { pulse, error }
}

function PulseStat({ tone, value, label }: { tone: string; value: number | undefined; label: string }) {
  return (
    <span className="flex items-baseline gap-1.5" title={`${label} ${value}`}>
      <span
        className="inline-block size-1.5 translate-y-[-1px] rounded-full"
        style={{ background: `var(--color-sig-${tone})` }}
        aria-hidden
      />
      <span className="num text-sm font-semibold text-ink-0">{value ?? '—'}</span>
      <span className="text-xs text-ink-2">{label}</span>
    </span>
  )
}

const navClass = 'tab'

export function AppShell({ children }: { children: ReactNode }) {
  const { user, logout, roles, expiresAt } = useAuth()
  const { dirty } = useWorkflow()
  const exit = useConfirmLayer()
  const { pulse, error: pulseError } = usePulse(user !== null, logout)
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (expiresAt === null) return
    const timer = window.setInterval(() => setNow(Date.now()), 30_000)
    return () => window.clearInterval(timer)
  }, [expiresAt])
  const remaining = expiresAt === null ? Infinity : new Date(expiresAt).getTime() - now

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
          <NavLink to="/approvals" className={navClass} data-testid={TID.navApprovals}>
            <I glyph={Stamp} />
            审批
          </NavLink>
        </nav>

        <div className="ml-auto flex items-center gap-5">
            <div className="hidden items-center gap-4 border-r border-line-0 pr-5 xl:flex" aria-label="运行脉冲" title="当前租户的执行计数；审批队列只列分配给当前账号的待办节点">
              <PulseStat tone="run" value={pulse?.running} label="执行中" />
              <PulseStat tone="wait" value={pulse?.waiting} label="待审批执行" />
              <PulseStat tone="fail" value={pulse?.failed} label="失败" />
              {pulseError !== '' ? <span role="status" className="text-xs text-ink-2" title={pulseError}>{pulse === null ? '脉冲未能读取' : '脉冲刷新失败 · 保留上次值'}</span> : pulse === null ? <span className="text-xs text-ink-2">正在读取</span> : null}
            </div>

          <div className="flex items-center gap-2">
            <ThemeControl className="mr-1" />
            <span className="mono text-xs text-ink-1" data-testid={TID.whoami} title="当前账号">
              {user}<span className="ml-2 text-ink-2">{roles.length > 0 ? roles.join(' / ') : '角色未知'}</span>
            </span>
            <button
              ref={exit.armRef}
              type="button"
              className="btn btn-quiet btn-sm btn-icon"
              onClick={() => { if (dirty) exit.arm(); else logout() }}
              title="退出登录"
              data-testid={TID.logout}
            >
              <I glyph={LogOut} size={13} />
              <span className="sr-only">退出登录</span>
            </button>
          </div>
        </div>
      </header>

      {roles.length === 0 ? <p className="border-b border-line-0 px-4 py-2 text-sm text-ink-2" role="status">当前会话缺少角色信息，只读浏览；请重新登录以恢复相应写操作。</p> : null}
      {remaining <= 5 * 60_000 && remaining > 0 ? <p className="border-b border-line-0 px-4 py-2 text-sm text-ink-1" role="status">会话即将到期，请保存当前工作；到期后需要重新登录。</p> : null}

      {exit.armed ? (
        <ConfirmLayer
          layerRef={exit.layerRef}
          label="退出前确认"
          hint="当前画布尚未保存，退出登录会丢弃这些更改。"
          confirmText="仍要退出"
          onConfirm={logout}
          onDismiss={exit.disarm}
        />
      ) : null}

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
