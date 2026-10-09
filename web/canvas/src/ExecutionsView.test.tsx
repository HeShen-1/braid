// ExecutionsView 测试(T21.1 Lane B):执行表(状态芯片 + 工作流 + 时长 +
// 创建)受控选中(onSelect 由路由承载)、触发面板(定义下拉 + JSON 输入 +
// 幂等键头 + 就地校验)、详情挂载、空态、401 上抛。fetch 全 stub 按 URL 路由。
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionsView } from './ExecutionsView.tsx'
import { tokenStore } from './api.ts'
import { TID, tid } from './ui/testids.ts'

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
  id: '7',
  definition_id: '1',
  definition_name: 'wf',
  definition_version: 1,
  status: 'RUNNING',
  trigger_type: 'API',
  triggered_by: 'author',
  error_code: null,
  error_message: null,
  created_at: '2026-10-04T06:00:00Z',
  started_at: null,
  finished_at: null,
  definition_hash: 'h',
  input: {},
  outputs: null,
  nodes: [],
  audit: { exec_date: '2026-10-04', trace_id: null },
}

const published = [{ id: '1', name: 'wf', version: 3, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z' }]

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('ExecutionsView(执行面)', () => {
  it('执行表格化:状态芯片 + 工作流版本 + 时长/创建;点行把 id 交给路由', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    stubRoute((url) => {
      if (url === '/api/v1/executions') {
        return ok({
          items: [
            { ...emptyDetail, id: '7', started_at: '2026-10-04T06:00:01Z' },
            { ...emptyDetail, id: '6', definition_name: 'other', status: 'SUCCEEDED', finished_at: '2026-10-04T06:00:09Z' },
          ],
          page: { limit: 20, offset: 0, total: 2 },
        })
      }
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: [] })
      if (url === '/api/v1/executions/6') return ok({ ...emptyDetail, id: '6', definition_name: 'other', status: 'SUCCEEDED' })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId="6" onSelect={onSelect} onAuthExpired={() => {}} />)

    const table = await screen.findByTestId(TID.execTable)
    const row7 = screen.getByTestId(tid.execRow('7'))
    expect(row7).toHaveTextContent('执行中')
    expect(row7).toHaveTextContent('wf')
    expect(row7).toHaveTextContent('v1')
    expect(screen.getByTestId(tid.execRow('6'))).toHaveAttribute('data-state', 'SUCCEEDED')
    // 选中态由 selectedId 受控(共享 .table 的 data-selected 样式)
    expect(screen.getByTestId(tid.execRow('6'))).toHaveAttribute('data-selected', 'true')
    expect(row7).toHaveAttribute('data-selected', 'false')
    expect(table).toBeInTheDocument()

    await user.click(row7)
    expect(onSelect).toHaveBeenCalledWith('7')
  })

  it('selectedId 非空 → 右栏挂载详情(芯片 data-state = 原始 ENUM)', async () => {
    stubRoute((url) => {
      if (url === '/api/v1/executions') return ok({ items: [{ ...emptyDetail, id: '7' }], page: { limit: 20, offset: 0, total: 1 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: [] })
      if (url === '/api/v1/executions/7') return ok(emptyDetail)
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId="7" onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.execDetail)).toBeInTheDocument()
    expect(await screen.findByTestId(TID.execStatus)).toHaveAttribute('data-state', 'RUNNING')
  })

  it('触发:选定义 + 填输入 → POST executions(Idempotency-Key 在场)→ 列表刷新 + 选中新执行', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    let triggered = false
    let sawKey = false
    stubRoute((url, init) => {
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: published })
      if (url === '/api/v1/executions' && init?.method === 'POST') {
        triggered = true
        const headers = init.headers as Record<string, string>
        sawKey = typeof headers['Idempotency-Key'] === 'string' && headers['Idempotency-Key'] !== ''
        expect(JSON.parse(String(init.body))).toEqual({ definition_name: 'wf', input: { docs: ['a'] } })
        return ok({ id: '8', definition_name: 'wf', definition_version: 3, status: 'PENDING', trigger_type: 'API', triggered_by: 'author' }, 201)
      }
      if (url === '/api/v1/executions') {
        return ok({
          items: triggered ? [{ ...emptyDetail, id: '8', status: 'PENDING' }] : [{ ...emptyDetail, id: '7' }],
          page: { limit: 20, offset: 0, total: 1 },
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await user.selectOptions(screen.getByTestId(TID.triggerDefinition), 'wf')
    // JSON 文本含 {/[ (user-event 键描述符元字符),直改值免转义
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '{"docs":["a"]}' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))

    await waitFor(() => expect(onSelect).toHaveBeenCalledWith('8'))
    expect(sawKey).toBe(true)
    expect(await screen.findByTestId(tid.execRow('8'))).toBeInTheDocument()
  })

  it('触发输入非法 JSON:原地报错不发请求', async () => {
    const user = userEvent.setup()
    let posted = 0
    stubRoute((url, init) => {
      if (url === '/api/v1/executions' && init?.method === 'POST') posted += 1
      if (url === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: published })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await user.selectOptions(screen.getByTestId(TID.triggerDefinition), 'wf')
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: 'not-json' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(await screen.findByRole('alert')).toHaveTextContent('输入不是合法 JSON')
    expect(posted).toBe(0)
  })

  it('触发输入顶层是数组:同样就地拦下', async () => {
    const user = userEvent.setup()
    stubRoute((url) => {
      if (url === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: published })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await user.selectOptions(screen.getByTestId(TID.triggerDefinition), 'wf')
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '[]' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(await screen.findByRole('alert')).toHaveTextContent('顶层必须是 JSON 对象')
  })

  it('无已发布工作流:触发禁用并给出下一步', async () => {
    stubRoute((url) => {
      if (url === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: [] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
    expect(screen.getByText(/尚无已发布工作流/)).toBeInTheDocument()
    expect(await screen.findByTestId(TID.execEmpty)).toBeInTheDocument()
  })

  it('无选中:右栏是引导空态(不猜数据)', async () => {
    stubRoute((url) => {
      if (url === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (url === '/api/v1/definitions?status=PUBLISHED') return ok({ items: [] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByText('未选择执行')).toBeInTheDocument()
    expect(screen.queryByTestId(TID.execDetail)).not.toBeInTheDocument()
  })

  it('列表 401 → onAuthExpired(统一登出)', async () => {
    const onAuthExpired = vi.fn()
    stubRoute((url) => {
      if (url === '/api/v1/executions') {
        return new Response(JSON.stringify({ error: 'unauthorized' }), {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return ok({ items: [] })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={onAuthExpired} />)
    await waitFor(() => expect(onAuthExpired).toHaveBeenCalled())
  })
})
