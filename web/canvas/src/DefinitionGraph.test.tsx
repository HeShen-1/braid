// DefinitionGraph 测试(T21.1 Lane B 增量):默认收起 = 零请求、展开按需拉取、
// 执行状态注入节点卡片(data-state)、失败就近错误条 + 重试、401 上抛、
// 零硬编码 hex(色相只走 token)。
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DefinitionGraph } from './DefinitionGraph.tsx'
import { tokenStore } from './api.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { tid } from './ui/testids.ts'

const DOC: WorkflowDoc = {
  dsl: '1.0',
  name: 'wf',
  nodes: [
    { id: 'gate', type: 'approval', title: '门一', approvers: ['alice'], mode: 'any', timeout_hours: 24, on_timeout: 'fail' },
    { id: 'read', type: 'tool', title: '读取', tool: 'file_read', inputs: { path: '{{item}}' } },
    { id: 'sign', type: 'approval', title: '会签', approvers: ['alice', 'bob'], mode: 'all', timeout_hours: 24, on_timeout: 'fail' },
  ],
  edges: [
    { from: 'gate', to: 'read' },
    { from: 'read', to: 'sign' },
  ],
}

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const VERSION_URL = '/api/v1/definitions/wf/versions/3'

/** 版本端点专线路由:其余 URL 一律不该被请求。 */
function stubVersion(responder: () => Response) {
  const fn = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url === VERSION_URL) return responder()
    return ok({ error: 'unexpected_url', url })
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

function versionCalls(fn: ReturnType<typeof stubVersion>): number {
  return fn.mock.calls.filter(([u]) => String(u) === VERSION_URL).length
}

const published = (dsl: WorkflowDoc = DOC) =>
  ok({ id: '1', name: 'wf', version: 3, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z', dsl })

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('DefinitionGraph(定义版本图)', () => {
  it('默认收起:零请求;展开才拉版本,并把执行状态注入节点卡片', async () => {
    const user = userEvent.setup()
    const fetchMock = stubVersion(() => published())
    render(
      <DefinitionGraph
        name="wf"
        version={3}
        statuses={{ gate: 'FAILED', read: 'EXECUTING' }}
        onAuthExpired={() => {}}
      />,
    )
    // 收起:标题给 name vX,图未挂载,且一次请求都没发
    expect(screen.getByText('wf v3')).toBeInTheDocument()
    expect(screen.queryByTestId(tid.nodeCard('gate'))).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    expect(await screen.findByTestId(tid.nodeCard('gate'))).toBeInTheDocument()
    expect(screen.getByTestId(tid.nodeCard('gate'))).toHaveAttribute('data-state', 'FAILED')
    expect(screen.getByTestId(tid.nodeCard('read'))).toHaveAttribute('data-state', 'EXECUTING')
    // 该节点这次没有执行行 → 不虚构状态
    expect(screen.getByTestId(tid.nodeCard('sign'))).not.toHaveAttribute('data-state')
    expect(versionCalls(fetchMock)).toBe(1)
    // 拓扑真的到了图上(节点数/边数来自拉回的定义版本)
    expect(screen.getByText(/3 节点 · 2 边/)).toBeInTheDocument()
    // 图里不注入 instanceNo:锚是 node-card,不与时间带的行锚重名
    expect(screen.queryByTestId(tid.nodeRow('gate', 0))).not.toBeInTheDocument()
  })

  it('收起再展开:命中已加载的定义,不重复请求', async () => {
    const user = userEvent.setup()
    const fetchMock = stubVersion(() => published())
    render(<DefinitionGraph name="wf" version={3} statuses={{}} onAuthExpired={() => {}} />)
    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    await screen.findByTestId(tid.nodeCard('gate'))
    await user.click(screen.getByRole('button', { name: '收起定义版本图' }))
    expect(screen.queryByTestId(tid.nodeCard('gate'))).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    expect(await screen.findByTestId(tid.nodeCard('gate'))).toBeInTheDocument()
    expect(versionCalls(fetchMock)).toBe(1)
  })

  it('加载失败:就近错误条 + 重试成功(不阻塞详情其余部分)', async () => {
    const user = userEvent.setup()
    let fail = true
    const fetchMock = stubVersion(() =>
      fail
        ? new Response(JSON.stringify({ error: 'not_found' }), { status: 404, headers: { 'Content-Type': 'application/json' } })
        : published(),
    )
    render(<DefinitionGraph name="wf" version={3} statuses={{}} onAuthExpired={() => {}} />)
    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('定义版本图加载失败')
    expect(alert).toHaveTextContent('not_found')

    fail = false
    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByTestId(tid.nodeCard('gate'))).toBeInTheDocument()
    expect(versionCalls(fetchMock)).toBe(2)
  })

  it('401:交给壳层登出,不在图里自说自话', async () => {
    const user = userEvent.setup()
    const onAuthExpired = vi.fn()
    stubVersion(() =>
      new Response(JSON.stringify({ error: 'unauthorized' }), { status: 401, headers: { 'Content-Type': 'application/json' } }),
    )
    render(<DefinitionGraph name="wf" version={3} statuses={{}} onAuthExpired={onAuthExpired} />)
    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    await vi.waitFor(() => expect(onAuthExpired).toHaveBeenCalled())
  })

  it('零硬编码 hex:色相/地面全部来自 token(源码锚)', () => {
    const src = readFileSync(resolve(import.meta.dirname, 'DefinitionGraph.tsx'), 'utf8')
    expect(src).not.toMatch(/#[0-9a-fA-F]{3,8}\b/)
  })
})
