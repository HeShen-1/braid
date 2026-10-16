import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { PublishedVersions } from './PublishedVersions.tsx'
import { AuthProvider, useAuth } from './auth.tsx'
import { session } from './session.ts'
import { tokenStore } from './api.ts'
import { CONSOLE_REFRESH } from './refresh.ts'

const old = { id: '1', name: 'archive-only', version: 1, status: 'ARCHIVED', updated_at: '2026-10-15T00:00:00Z', description: '' }
const latest = { ...old, id: '2', name: 'wf', version: 2, status: 'PUBLISHED' }
const ok = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
function stub() {
  let archived = false
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/archive') && init?.method === 'POST') { archived = true; return ok({ ...latest, status: 'ARCHIVED', dsl: { name: 'wf' } }) }
    if (url.includes('status=PUBLISHED')) return ok({ items: [latest] })
    if (url.includes('status=ARCHIVED')) return ok({ items: [old] })
    if (url.endsWith('/versions')) return ok({ items: url.includes('archive-only') ? [old] : [latest] })
    return ok({ ...(url.includes('archive-only') ? old : { ...latest, status: archived ? 'ARCHIVED' : 'PUBLISHED' }), dsl: { name: url.includes('archive-only') ? old.name : latest.name, nodes: [{ id: 'gate', type: 'approval' }] } })
  })
  vi.stubGlobal('fetch', fn)
  return fn
}
afterEach(() => { session.clear(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

function SessionSwitch() {
  const auth = useAuth()
  const [shown, setShown] = useState(true)
  return <><output aria-label="当前用户">{auth.user ?? '已退出'}</output><button onClick={() => { setShown(false); auth.logout(); void auth.login('new-user', 'test-password') }}>切换到新会话</button>{shown ? <PublishedVersions canManage={auth.canAuthor} onAuthExpired={auth.logout} /> : null}</>
}

describe('定义版本浏览', () => {
  it('另端归档后刷新同步已打开定义的真实状态，不再与版本行矛盾', async () => {
    let archived = false
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      const row = { ...latest, status: archived ? 'ARCHIVED' : 'PUBLISHED' }
      if (url.includes('status=PUBLISHED')) return ok({ items: archived ? [] : [row] })
      if (url.includes('status=ARCHIVED')) return ok({ items: archived ? [row] : [] })
      if (url.endsWith('/versions')) return ok({ items: [row] })
      return ok({ ...row, dsl: { name: 'wf', description: '不可变版本定义' } })
    }))
    const user = userEvent.setup()
    render(<PublishedVersions canManage={false} onAuthExpired={() => {}} />)
    await user.click(await screen.findByRole('button', { name: '查看 v2 定义' }))
    await screen.findByRole('heading', { name: 'wf v2 · 已发布' })
    archived = true
    await user.click(screen.getByRole('button', { name: '刷新版本' }))
    await waitFor(() => expect(within(screen.getByRole('table')).getByText('已归档')).toBeInTheDocument())
    expect(screen.getByRole('heading', { name: 'wf v2 · 已归档' })).toBeInTheDocument()
    expect(screen.getByLabelText('版本定义')).toHaveTextContent('不可变版本定义')
  })

  it('刷新版本后迟到的旧定义响应不能复活过时发布状态', async () => {
    let archived = false
    let finish!: (response: Response) => void
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      const row = { ...latest, status: archived ? 'ARCHIVED' : 'PUBLISHED' }
      if (url.includes('status=PUBLISHED')) return ok({ items: archived ? [] : [row] })
      if (url.includes('status=ARCHIVED')) return ok({ items: archived ? [row] : [] })
      if (url.endsWith('/versions')) return ok({ items: [row] })
      return new Promise<Response>((resolve) => { finish = resolve })
    }))
    const user = userEvent.setup()
    render(<PublishedVersions canManage={false} onAuthExpired={() => {}} />)
    await user.click(await screen.findByRole('button', { name: '查看 v2 定义' }))
    archived = true
    await user.click(screen.getByRole('button', { name: '刷新版本' }))
    await waitFor(() => expect(within(screen.getByRole('table')).getByText('已归档')).toBeInTheDocument())
    await act(async () => { finish(ok({ ...latest, dsl: { name: 'old-response' } })) })
    expect(screen.queryByRole('heading', { name: 'wf v2 · 已发布' })).not.toBeInTheDocument()
    expect(screen.queryByText(/old-response/)).not.toBeInTheDocument()
  })
  it.each([200, 401])('卸载并重新登录后，旧归档响应%i既不清新会话也不广播刷新', async (status) => {
    session.start('old-token', 'old-user', ['author'])
    const fetcher = stub()
    const original = fetcher.getMockImplementation()!
    let finish!: (response: Response) => void
    fetcher.mockImplementation(async (input, init) => {
      const url = String(input)
      if (url.endsWith('/auth/login')) return ok({ token: 'new-token', username: 'new-user', roles: ['author'], expires_at: '2999-01-01T00:00:00Z' })
      if (url.endsWith('/archive') && init?.method === 'POST') return new Promise<Response>((resolve) => { finish = resolve })
      return original(input, init)
    })
    const events = vi.spyOn(window, 'dispatchEvent')
    const user = userEvent.setup()
    render(<AuthProvider><SessionSwitch /></AuthProvider>)
    await screen.findByRole('button', { name: '查看 v1 定义' })
    await user.click(screen.getByRole('combobox', { name: '工作流' }))
    await user.click(await screen.findByRole('option', { name: 'wf' }))
    await user.click(await screen.findByRole('button', { name: '归档 v2' }))
    await user.click(screen.getByRole('button', { name: '确认归档 v2' }))
    await user.click(screen.getByRole('button', { name: '切换到新会话' }))
    await screen.findByText('new-user')
    await act(async () => { finish(ok(status === 401 ? { error: 'unauthorized' } : { ...latest, status: 'ARCHIVED' }, status)) })
    expect(screen.getByLabelText('当前用户')).toHaveTextContent('new-user')
    expect(tokenStore.get()).toBe('new-token')
    expect(events.mock.calls.filter(([event]) => event.type === CONSOLE_REFRESH)).toHaveLength(0)
  })

  it.each([200, 401])('页面仍在但当前凭证已更换时，旧归档响应%i不回填、不失效新凭证', async (status) => {
    session.start('old-token', 'old-user', ['author'])
    const fetcher = stub()
    const original = fetcher.getMockImplementation()!
    let finish!: (response: Response) => void
    fetcher.mockImplementation(async (input, init) => String(input).endsWith('/archive') && init?.method === 'POST' ? new Promise<Response>((resolve) => { finish = resolve }) : original(input, init))
    const expired = vi.fn(() => session.clear())
    const events = vi.spyOn(window, 'dispatchEvent')
    const user = userEvent.setup()
    render(<PublishedVersions canManage onAuthExpired={expired} />)
    await screen.findByRole('button', { name: '查看 v1 定义' })
    await user.click(screen.getByRole('combobox', { name: '工作流' }))
    await user.click(await screen.findByRole('option', { name: 'wf' }))
    await user.click(await screen.findByRole('button', { name: '归档 v2' }))
    await user.click(screen.getByRole('button', { name: '确认归档 v2' }))
    session.start('new-token', 'new-user', ['author'])
    await act(async () => { finish(ok(status === 401 ? { error: 'unauthorized' } : { ...latest, status: 'ARCHIVED' }, status)) })
    expect(tokenStore.get()).toBe('new-token')
    expect(expired).not.toHaveBeenCalled()
    expect(screen.queryByText('已归档 wf v2；已有执行不受影响。')).not.toBeInTheDocument()
    expect(events.mock.calls.filter(([event]) => event.type === CONSOLE_REFRESH)).toHaveLength(0)
  })
  it.each(['names', 'versions'] as const)('已布防时%s刷新在飞或失败均阻断归档POST，失败后仍可放弃', async (face) => {
    const fetcher = stub()
    const original = fetcher.getMockImplementation()!
    let failRefresh = false
    let finish!: (response: Response) => void
    fetcher.mockImplementation(async (input, init) => {
      const url = String(input)
      if (failRefresh && (face === 'names' ? url.includes('status=ARCHIVED') : url.endsWith('/wf/versions'))) return new Promise<Response>((resolve) => { finish = resolve })
      return original(input, init)
    })
    const user = userEvent.setup()
    render(<PublishedVersions canManage onAuthExpired={() => {}} />)
    await screen.findByRole('button', { name: '查看 v1 定义' })
    await user.click(screen.getByRole('combobox', { name: '工作流' }))
    await user.click(await screen.findByRole('option', { name: 'wf' }))
    await user.click(await screen.findByRole('button', { name: '归档 v2' }))
    failRefresh = true
    await user.click(screen.getByRole('button', { name: '刷新版本' }))
    const confirm = screen.getByRole('button', { name: '确认归档 v2' })
    expect(confirm).toBeDisabled()
    fireEvent.click(confirm)
    await act(async () => { finish(ok({ error: 'internal' }, 500)) })
    expect(await screen.findByRole('alert')).toHaveTextContent('版本操作未完成')
    expect(confirm).toBeDisabled()
    fireEvent.click(confirm)
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: '放弃' }))
    expect(screen.queryByRole('group')).not.toBeInTheDocument()
  })
  it('只读角色仍能找到全归档名字并检查真实定义，零归档写入口', async () => {
    const fetcher = stub()
    const user = userEvent.setup()
    render(<PublishedVersions canManage={false} onAuthExpired={() => {}} />)
    const row = await screen.findByRole('button', { name: '查看 v1 定义' })
    await user.click(row)
    expect(await screen.findByRole('heading', { name: 'archive-only v1 · 已归档' })).toBeInTheDocument()
    expect(screen.getByLabelText('版本定义')).toHaveTextContent('"type": "approval"')
    expect(screen.queryByRole('button', { name: /^归档/ })).not.toBeInTheDocument()
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
  })

  it('author 归档需确认；Esc 回按钮且零POST；确认仅一次并保留历史定义', async () => {
    const fetcher = stub()
    const user = userEvent.setup()
    render(<PublishedVersions canManage onAuthExpired={() => {}} />)
    await screen.findByRole('button', { name: '查看 v1 定义' })
    await user.click(screen.getByRole('combobox', { name: '工作流' }))
    await user.click(await screen.findByRole('option', { name: 'wf' }))
    await user.click(await screen.findByRole('button', { name: '归档 v2' }))
    expect(screen.getByRole('group', { name: '归档 wf v2' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('button', { name: '归档 v2' })).toHaveFocus()
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: '归档 v2' }))
    await user.dblClick(screen.getByRole('button', { name: '确认归档 v2' }))
    await screen.findByText('已归档 wf v2；已有执行不受影响。')
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
    expect(within(screen.getByRole('table')).getByText('已归档')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '查看 v2 定义' }))
    expect(await screen.findByRole('heading', { name: 'wf v2 · 已归档' })).toBeInTheDocument()
  })

  it.each([403, 404, 500])('版本读取 %i 明确失败，保留版本列表以继续选择', async (status) => {
    const fetcher = stub()
    const original = fetcher.getMockImplementation()!
    fetcher.mockImplementation(async (input, init) => /\/versions\/\d+$/.test(String(input)) ? ok({ error: status === 403 ? 'role_denied' : 'not_found' }, status) : original(input, init))
    render(<PublishedVersions canManage={false} onAuthExpired={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: '查看 v1 定义' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('版本操作未完成')
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.queryByLabelText('版本定义')).not.toBeInTheDocument()
  })

  it('换名字后迟到的旧版本定义不能串号到新选择', async () => {
    const fetcher = stub()
    const original = fetcher.getMockImplementation()!
    let finish!: (r: Response) => void
    fetcher.mockImplementation(async (input, init) => String(input).endsWith('/archive-only/versions/1') ? new Promise<Response>((r) => { finish = r }) : original(input, init))
    const user = userEvent.setup()
    render(<PublishedVersions canManage={false} onAuthExpired={() => {}} />)
    await user.click(await screen.findByRole('button', { name: '查看 v1 定义' }))
    await user.click(screen.getByRole('combobox', { name: '工作流' }))
    await user.click(await screen.findByRole('option', { name: 'wf' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '查看 v2 定义' })).toBeInTheDocument())
    await act(async () => { finish(ok({ ...old, dsl: { name: 'old-response' } })) })
    expect(screen.queryByLabelText('版本定义')).not.toBeInTheDocument()
  })
})
