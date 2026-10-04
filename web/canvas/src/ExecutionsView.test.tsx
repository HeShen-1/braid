// ExecutionsView 组件测试(T16.6):列表渲染/选中、触发面板(定义下拉 +
// 输入 JSON + 幂等键头)、视图切换后详情加载。fetch 全 stub 按 URL 路由。
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionsView } from './ExecutionsView.tsx'
import { tokenStore } from './api.ts'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubRoute(responder: (url: string, init?: RequestInit) => Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => responder(String(input), init)),
  )
}

const emptyDetail = {
  id: '7', definition_id: '1', definition_name: 'wf', definition_version: 1, status: 'RUNNING',
  trigger_type: 'API', triggered_by: 'author', error_code: null, error_message: null,
  created_at: '2026-10-04T06:00:00Z', started_at: null, finished_at: null,
  definition_hash: 'h', input: {}, outputs: null, nodes: [], audit: { exec_date: '2026-10-04', trace_id: null },
}

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('ExecutionsView(执行视图)', () => {
  it('列表渲染 + 点行选中加载详情', async () => {
    const user = userEvent.setup()
    stubRoute((url) => {
      if (url === '/api/v1/executions') {
        return ok({
          items: [
            { ...emptyDetail, id: '7' },
            { ...emptyDetail, id: '6', definition_name: 'other', status: 'SUCCEEDED' },
          ],
          page: { limit: 20, offset: 0, total: 2 },
        })
      }
      if (url === '/api/v1/executions/7') return ok({ ...emptyDetail, nodes: [] })
      if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
      if (/\/versions\/\d+$/.test(url)) return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: [] })
      return ok({ error: 'unexpected', url })
    })
    render(<ExecutionsView onAuthExpired={() => {}} />)
    expect(await screen.findByTestId('exec-row-7')).toBeInTheDocument()
    expect(screen.getByTestId('exec-row-6')).toHaveTextContent('other')
    await user.click(screen.getByTestId('exec-row-7'))
    await waitFor(() => expect(screen.getByTestId('exec-detail')).toBeInTheDocument())
  })

  it('触发面板:选定义 + 填输入 → POST executions(Idempotency-Key 在场)→ 选中新执行', async () => {
    const user = userEvent.setup()
    let triggered = false
    let sawKey = false
    stubRoute((url, init) => {
      if (url === '/api/v1/definitions?status=PUBLISHED') {
        return ok({ items: [{ id: '1', name: 'wf', version: 3, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z' }] })
      }
      if (url === '/api/v1/executions' && init?.method === 'POST') {
        triggered = true
        const headers = init.headers as Record<string, string>
        sawKey = typeof headers['Idempotency-Key'] === 'string' && headers['Idempotency-Key'] !== ''
        expect(JSON.parse(String(init.body))).toEqual({ definition_name: 'wf', input: { docs: ['a'] } })
        return ok({ id: '8', definition_name: 'wf', definition_version: 3, status: 'PENDING', trigger_type: 'API', triggered_by: 'author' }, 201)
      }
      if (url === '/api/v1/executions') {
        return ok({
          items: triggered
            ? [{ ...emptyDetail, id: '8', status: 'PENDING' }]
            : [{ ...emptyDetail, id: '7' }],
          page: { limit: 20, offset: 0, total: 1 },
        })
      }
      if (url === '/api/v1/executions/8') return ok({ ...emptyDetail, id: '8', status: 'PENDING', nodes: [] })
      if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
      if (/\/versions\/\d+$/.test(url)) return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      return ok({ error: 'unexpected', url })
    })
    render(<ExecutionsView onAuthExpired={() => {}} />)
    await screen.findByTestId('trigger-definition')
    await user.selectOptions(screen.getByTestId('trigger-definition'), 'wf')
    // JSON 文本含 {/[ (user-event 键描述符元字符),直改值免转义
    fireEvent.change(screen.getByTestId('trigger-input'), { target: { value: '{"docs":["a"]}' } })
    await user.click(screen.getByTestId('trigger-submit'))
    await waitFor(() => expect(screen.getByTestId('exec-detail')).toBeInTheDocument())
    expect(sawKey).toBe(true)
  })

  it('触发输入非法 JSON:原地报错不发请求', async () => {
    const user = userEvent.setup()
    let posted = 0
    stubRoute((url, init) => {
      if (url === '/api/v1/executions' && init?.method === 'POST') posted += 1
      if (url === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') {
        return ok({ items: [{ id: '1', name: 'wf', version: 1, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z' }] })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView onAuthExpired={() => {}} />)
    await screen.findByTestId('trigger-definition')
    await user.selectOptions(screen.getByTestId('trigger-definition'), 'wf')
    fireEvent.change(screen.getByTestId('trigger-input'), { target: { value: 'not-json' } })
    await user.click(screen.getByTestId('trigger-submit'))
    expect(await screen.findByTestId('trigger-error')).toBeInTheDocument()
    expect(posted).toBe(0)
  })
})
