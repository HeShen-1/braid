import { act, render, screen, within } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, RequireAuth } from '../auth.tsx'
import { session } from '../session.ts'
import { refreshConsole } from '../refresh.ts'
import { AppLayout } from './AppLayout.tsx'

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
function mount() {
  const router = createMemoryRouter([
    { element: <RequireAuth><AppLayout /></RequireAuth>, children: [{ path: '/workflows', element: <span>内容</span> }] },
    { path: '/login', element: <span>会话已退出</span> },
  ], { initialEntries: ['/workflows'] })
  render(<AuthProvider><RouterProvider router={router} /></AuthProvider>)
}
afterEach(() => { session.clear(); vi.unstubAllGlobals(); vi.useRealTimers() })

describe('运行脉冲与会话提示', () => {
  it('500保留上次真实计数，事件立即刷新；计数说明明确个人队列与租户执行不同', async () => {
    session.start('t', 'reader', ['approver'])
    let status = 200
    let total = 7
    const fetcher = vi.fn(async (input: RequestInfo | URL) => String(input).includes('/executions?')
      ? response(status === 200 ? { items: [], page: { total } } : { error: 'internal' }, status)
      : response({ items: [] }))
    vi.stubGlobal('fetch', fetcher)
    mount()
    const pulse = screen.getByLabelText('运行脉冲')
    expect(pulse).toHaveTextContent('—')
    await act(async () => {})
    expect(within(pulse).getAllByText('7')).toHaveLength(3)
    expect(pulse).toHaveAttribute('title', expect.stringContaining('当前账号'))
    status = 500
    await act(async () => { refreshConsole() })
    expect(pulse).toHaveTextContent('脉冲刷新失败 · 保留上次值')
    expect(within(pulse).getAllByText('7')).toHaveLength(3)
    status = 200; total = 8
    await act(async () => { refreshConsole() })
    expect(within(pulse).getAllByText('8')).toHaveLength(3)
    expect(pulse).not.toHaveTextContent('刷新失败')
  })

  it('首次失败显示未知，不伪装成零计数', async () => {
    session.start('t', 'reader', ['approver'])
    vi.stubGlobal('fetch', vi.fn(async () => response({ error: 'internal' }, 500)))
    mount()
    await act(async () => {})
    const pulse = screen.getByLabelText('运行脉冲')
    expect(within(pulse).getAllByText('—')).toHaveLength(3)
    expect(pulse).toHaveTextContent('脉冲未能读取')
  })

  it('同时出现500/401时仍统一清除会话，不冻结旧计数', async () => {
    session.start('t', 'reader', ['approver'])
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).includes('status=RUNNING') ? response({ error: 'internal' }, 500) : String(input).includes('/executions?') ? response({ error: 'unauthorized' }, 401) : response({ items: [] })))
    mount()
    await act(async () => {})
    expect(screen.getByText('会话已退出')).toBeInTheDocument()
    expect(session.authed).toBe(false)
  })

  it('旧角色信息缺失只读提示与真实临期提示', async () => {
    session.start('t', 'reader', [], new Date(Date.now() + 60_000).toISOString())
    vi.stubGlobal('fetch', vi.fn(async () => response({ items: [], page: { total: 0 } })))
    mount()
    await act(async () => {})
    expect(screen.getByText(/当前会话缺少角色信息/)).toBeInTheDocument()
    expect(screen.getByText(/会话即将到期/)).toBeInTheDocument()
  })
})
