// ExecutionsView 测试(T21.1 Lane B):执行表(状态芯片 + 工作流 + 时长 +
// 创建)受控选中(onSelect 由路由承载)、服务端筛选(状态 + 工作流名,换筛选
// 重置 offset)、翻页、筛选专属空态与清除出口、触发面板(定义下拉 + JSON
// 输入 + 幂等键头 + 就地校验)、401 上抛。fetch 全 stub 按 URL 路由。
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent, { type UserEvent } from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionsView } from './ExecutionsView.tsx'
import { tokenStore } from './api.ts'
import { TID, tid } from './ui/testids.ts'

/** 决策 3 收敛后 Select 不再是原生控件:两步交互 = 点触发器 → 点选项。 */
async function chooseOption(user: UserEvent, trigger: HTMLElement, option: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name: option }))
}

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubRoute(
  responder: (url: string, init?: RequestInit) => Response | Promise<Response>,
  onVersion?: (url: string) => Response | Promise<Response>,
) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const version = url.match(/^\/api\/v1\/definitions\/([^/]+)\/versions\/(\d+)$/)
    if (version !== null) return onVersion?.(url) ?? ok({
      ...published[0], name: version[1], version: Number(version[2]),
      dsl: { dsl: '1.0', name: version[1], nodes: [], inputs: [] },
    })
    return responder(url, init)
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

/** 列表请求恒带 limit/offset:断言与路由都按 path + query 拆开读。 */
function pathOf(url: string): string {
  return url.split('?')[0]
}

function queryOf(url: string): URLSearchParams {
  return new URLSearchParams(url.split('?')[1] ?? '')
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

function exec(id: string, status: string, name = 'wf') {
  return { ...emptyDetail, id, status, definition_name: name }
}

const published = [{ id: '1', name: 'wf', version: 3, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z' }]

/** 最小路由:列表 + 已发布定义;并交出「列表请求 URL」序列供筛选断言。 */
function listStub(onList: (q: URLSearchParams) => Response) {
  const fn = stubRoute((url, init) => {
    const path = pathOf(url)
    if (path === '/api/v1/executions') {
      if (init?.method === 'POST') {
        return ok({ id: '8', definition_name: 'wf', definition_version: 3, status: 'PENDING', trigger_type: 'API', triggered_by: 'author' }, 201)
      }
      return onList(queryOf(url))
    }
    if (path === '/api/v1/definitions') return ok({ items: published })
    return ok({ items: [], next_cursor: '' })
  })
  const listUrls = () => fn.mock.calls.map(([u]) => String(u)).filter((u) => pathOf(u) === '/api/v1/executions')
  return { fn, listUrls }
}

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('ExecutionsView(执行面)', () => {
  it('所选版本展示类型/必填/说明和JSON示例,缺字段先就地提示,零假值可提交', async () => {
    const user = userEvent.setup()
    const posted: unknown[] = []
    const inputs = [
      { name: 'enabled', type: 'boolean', required: true, description: '是否启用检查' },
      { name: 'count', type: 'number', required: true },
      { name: 'items', type: 'array', required: true },
      { name: 'options', type: 'object', required: true },
      { name: 'note', type: 'string', required: false },
    ]
    stubRoute((url, init) => {
      if (pathOf(url) === '/api/v1/definitions') return ok({ items: published })
      if (init?.method === 'POST') {
        posted.push(JSON.parse(String(init.body)))
        return ok({ id: '8' }, 201)
      }
      return ok({ items: [], page: { total: 0 } })
    }, () => ok({ ...published[0], dsl: { inputs } }))
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    const contract = screen.getByLabelText('所选版本输入要求')
    expect(contract).toHaveTextContent('输入要求 · wf v3')
    expect(contract).toHaveTextContent('boolean · 必填')
    expect(contract).toHaveTextContent('string · 选填')
    expect(contract).toHaveTextContent('是否启用检查')
    await user.click(screen.getByText('输入 JSON 示例'))
    expect(contract.querySelector('pre')?.textContent).toContain('"enabled": false')
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(await screen.findByRole('alert')).toHaveTextContent('缺少必填参数：enabled、count、items、options')
    expect(posted).toEqual([])
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '{"enabled":false,"count":0,"items":[],"options":{}}' } })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await user.click(screen.getByTestId(TID.triggerSubmit))
    await waitFor(() => expect(posted).toEqual([{ definition_name: 'wf', version: 3, input: { enabled: false, count: 0, items: [], options: {} } }]))
  })

  it('迟到的旧版输入契约不覆盖新选择;列表出现新版时不偷换契约', async () => {
    const user = userEvent.setup()
    let newest = 3
    let finishOld!: (response: Response) => void
    const pending = new Promise<Response>((resolve) => { finishOld = resolve })
    const versions: string[] = []
    stubRoute((url) => {
      if (pathOf(url) === '/api/v1/definitions') return ok({ items: [{ ...published[0], id: String(newest), version: newest }] })
      return ok({ items: [], page: { total: 0 } })
    }, (url) => {
      versions.push(url)
      return url.endsWith('/3') ? pending : ok({ ...published[0], version: 4, dsl: { inputs: [{ name: 'new_field', type: 'string', required: true }] } })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
    expect(screen.getByRole('status')).toHaveTextContent('wf v3 的输入要求')
    newest = 4
    await user.click(screen.getByRole('button', { name: '刷新执行列表' }))
    await waitFor(() => expect(versions).toEqual(['/api/v1/definitions/wf/versions/3']))
    expect(screen.getByTestId(TID.triggerDefinition)).toHaveTextContent('wf v3')
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'wf v4')
    expect(await screen.findByText('new_field')).toBeInTheDocument()
    finishOld(ok({ ...published[0], dsl: { inputs: [{ name: 'old_field', type: 'number', required: true }] } }))
    await waitFor(() => expect(screen.getByTestId(TID.triggerSubmit)).toBeEnabled())
    expect(screen.getByLabelText('所选版本输入要求')).toHaveTextContent('wf v4')
    expect(screen.queryByText('old_field')).not.toBeInTheDocument()
    expect(versions).toEqual(['/api/v1/definitions/wf/versions/3', '/api/v1/definitions/wf/versions/4'])
  })

  it('读取输入要求失败不冒充空契约,可重试且保留用户输入', async () => {
    const user = userEvent.setup()
    let failed = true
    stubRoute((url) => pathOf(url) === '/api/v1/definitions' ? ok({ items: published }) : ok({ items: [], page: { total: 0 } }),
      () => failed ? ok({ error: 'http_503' }, 503) : ok({ ...published[0], dsl: { inputs: [] } }))
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '{"preserved":true}' } })
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    expect(await screen.findByRole('alert')).toHaveTextContent('读取 wf v3 的输入要求失败，请重试后再触发')
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
    expect(screen.queryByText(/未声明触发参数/)).not.toBeInTheDocument()
    failed = false
    await user.click(screen.getByRole('button', { name: '重试读取输入要求' }))
    expect(await screen.findByText(/此版本未声明触发参数/)).toBeInTheDocument()
    expect(screen.getByTestId(TID.triggerInput)).toHaveValue('{"preserved":true}')
    expect(screen.getByTestId(TID.triggerSubmit)).toBeEnabled()
  })

  it.each([401, 404])('输入契约读取 %i:登出或失效出口,不触发', async (status) => {
    const user = userEvent.setup()
    const expired = vi.fn()
    stubRoute((url) => pathOf(url) === '/api/v1/definitions' ? ok({ items: published }) : ok({ items: [], page: { total: 0 } }),
      () => ok({ error: status === 401 ? 'unauthorized' : 'not_found' }, status))
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={expired} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    if (status === 401) await waitFor(() => expect(expired).toHaveBeenCalledTimes(1))
    else expect(await screen.findByRole('alert')).toHaveTextContent('wf v3 已失效或不存在，请刷新后重新选择')
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
  })

  it('执行表格化:状态芯片 + 工作流版本 + 时长/创建;点行把 id 交给路由', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    stubRoute((url) => {
      const path = pathOf(url)
      if (path === '/api/v1/executions') {
        return ok({ items: [exec('7', 'RUNNING'), exec('6', 'SUCCEEDED', 'other')], page: { limit: 20, offset: 0, total: 2 } })
      }
      if (path === '/api/v1/executions/6') return ok(exec('6', 'SUCCEEDED', 'other'))
      if (path === '/api/v1/definitions') return ok({ items: [] })
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
      const path = pathOf(url)
      if (path === '/api/v1/executions') return ok({ items: [exec('7', 'RUNNING')], page: { limit: 20, offset: 0, total: 1 } })
      if (path === '/api/v1/executions/7') return ok(emptyDetail)
      if (path === '/api/v1/definitions') return ok({ items: [] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId="7" onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.execDetail)).toBeInTheDocument()
    expect(await screen.findByTestId(TID.execStatus)).toHaveAttribute('data-state', 'RUNNING')
  })

  it('状态筛选:选中即请求带 status(未筛选 = 不带该参数)', async () => {
    const user = userEvent.setup()
    const { listUrls } = listStub((q) =>
      q.get('status') === 'FAILED'
        ? ok({ items: [exec('3', 'FAILED')], page: { limit: 20, offset: 0, total: 1 } })
        : ok({ items: [exec('7', 'RUNNING'), exec('6', 'SUCCEEDED')], page: { limit: 20, offset: 0, total: 2 } }),
    )
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.execRow('7'))
    expect(queryOf(listUrls()[0]).has('status')).toBe(false)

    await chooseOption(user, screen.getByLabelText('按状态'), '已失败')
    expect(await screen.findByTestId(tid.execRow('3'))).toBeInTheDocument()
    expect(screen.queryByTestId(tid.execRow('7'))).not.toBeInTheDocument()
    const last = queryOf(listUrls().at(-1) as string)
    expect(last.get('status')).toBe('FAILED')
    expect(last.get('offset')).toBe('0')
  })

  it('工作流名筛选:防抖后请求带 name', async () => {
    const { listUrls } = listStub((q) =>
      q.get('name') === 'other'
        ? ok({ items: [exec('6', 'SUCCEEDED', 'other')], page: { limit: 20, offset: 0, total: 1 } })
        : ok({ items: [exec('7', 'RUNNING')], page: { limit: 20, offset: 0, total: 1 } }),
    )
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.execRow('7'))
    fireEvent.change(screen.getByLabelText('按名称'), { target: { value: 'other' } })
    await waitFor(() => expect(queryOf(listUrls().at(-1) as string).get('name')).toBe('other'))
    expect(await screen.findByTestId(tid.execRow('6'))).toBeInTheDocument()
  })

  it('换筛选重置 offset:第 2 页上切状态 → 下一次请求回第 1 页', async () => {
    const user = userEvent.setup()
    const { listUrls } = listStub((q) => {
      const off = Number(q.get('offset') ?? '0')
      if (q.get('status') === 'SUCCEEDED') {
        return ok({ items: [exec('99', 'SUCCEEDED')], page: { limit: 20, offset: 0, total: 1 } })
      }
      return ok({
        items: Array.from({ length: 20 }, (_, i) => exec(String(off + i), 'RUNNING')),
        page: { limit: 20, offset: off, total: 45 },
      })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.execRow('0'))
    expect(screen.getByText(/1–20 \/ 45/)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '下一页' }))
    await waitFor(() => expect(queryOf(listUrls().at(-1) as string).get('offset')).toBe('20'))
    expect(await screen.findByTestId(tid.execRow('20'))).toBeInTheDocument()

    await chooseOption(user, screen.getByLabelText('按状态'), '已成功')
    await waitFor(() => expect(queryOf(listUrls().at(-1) as string).get('status')).toBe('SUCCEEDED'))
    expect(queryOf(listUrls().at(-1) as string).get('offset')).toBe('0')
  })

  it('筛选无结果:专属空态(与「暂无执行」区分)+ 清除筛选出口', async () => {
    const user = userEvent.setup()
    listStub((q) =>
      q.get('status') === 'FAILED'
        ? ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
        : ok({ items: [exec('7', 'RUNNING')], page: { limit: 20, offset: 0, total: 1 } }),
    )
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.execRow('7'))

    await chooseOption(user, screen.getByLabelText('按状态'), '已失败')
    const empty = await screen.findByTestId(TID.execEmpty)
    expect(empty).toHaveTextContent('当前筛选下没有执行')
    expect(empty).not.toHaveTextContent('暂无执行')

    await user.click(within(empty).getByRole('button', { name: '清除筛选' }))
    expect(await screen.findByTestId(tid.execRow('7'))).toBeInTheDocument()
    expect(screen.getByLabelText('按状态')).toHaveTextContent('全部状态')
  })

  it('触发:选定义 + 填输入 → POST executions(Idempotency-Key 在场)→ 列表刷新 + 选中新执行', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    let triggered = false
    let sawKey = false
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/definitions') return ok({ items: published })
      if (path === '/api/v1/executions' && init?.method === 'POST') {
        triggered = true
        const headers = init.headers as Record<string, string>
        sawKey = typeof headers['Idempotency-Key'] === 'string' && headers['Idempotency-Key'] !== ''
        expect(JSON.parse(String(init.body))).toEqual({ definition_name: 'wf', version: 3, input: { docs: ['a'] } })
        return ok({ id: '8', definition_name: 'wf', definition_version: 3, status: 'PENDING', trigger_type: 'API', triggered_by: 'author' }, 201)
      }
      if (path === '/api/v1/executions') {
        return ok({
          items: triggered ? [exec('8', 'PENDING')] : [exec('7', 'RUNNING')],
          page: { limit: 20, offset: 0, total: 1 },
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'wf v3')
    // JSON 文本含 {/[ (user-event 键描述符元字符),直改值免转义
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '{"docs":["a"]}' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))

    await waitFor(() => expect(onSelect).toHaveBeenCalledWith('8'))
    expect(sawKey).toBe(true)
    expect(await screen.findByTestId(tid.execRow('8'))).toBeInTheDocument()
  })

  it('列表刷新发现新版本仍保留原选择;只有重新选择才更换触发版本', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    let newest = 3
    const posted: unknown[] = []
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/definitions') return ok({ items: [published[0], { ...published[0], id: String(newest), version: newest }] })
      if (path === '/api/v1/executions' && init?.method === 'POST') {
        posted.push(JSON.parse(String(init.body)))
        return ok({ id: '8', definition_name: 'wf', definition_version: newest, status: 'PENDING' }, 201)
      }
      return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
    })
    render(<ExecutionsView selectedId={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    newest = 4
    await user.click(screen.getByRole('button', { name: '刷新执行列表' }))
    await user.click(screen.getByTestId(TID.triggerDefinition))
    await screen.findByRole('option', { name: 'wf v4' })
    expect(screen.getByRole('option', { name: 'wf v3' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.getByTestId(TID.triggerDefinition)).toHaveTextContent('wf v3')
    await user.click(screen.getByTestId(TID.triggerSubmit))
    await waitFor(() => expect(posted).toEqual([{ definition_name: 'wf', version: 3, input: {} }]))
    await waitFor(() => expect(screen.getByTestId(TID.triggerSubmit)).toBeEnabled())
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'wf v4')
    await user.click(screen.getByTestId(TID.triggerSubmit))
    await waitFor(() => expect(posted[1]).toEqual({ definition_name: 'wf', version: 4, input: {} }))
  })

  it.each([
    [409, 'conflict', '已归档或不可执行'],
    [404, 'not_found', '已失效或不存在'],
  ])('所选版本失败 %i %s:明确反馈,不降级重试 latest', async (status, code, message) => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const posted: unknown[] = []
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/definitions') return ok({ items: published })
      if (path === '/api/v1/executions' && init?.method === 'POST') {
        posted.push(JSON.parse(String(init.body)))
        return ok({ error: code }, status)
      }
      return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
    })
    render(<ExecutionsView selectedId={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    await user.click(screen.getByTestId(TID.triggerSubmit))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('wf v3')
    expect(alert).toHaveTextContent(message)
    expect(alert).toHaveTextContent(code)
    expect(alert).toHaveTextContent('重新选择')
    expect(posted).toEqual([{ definition_name: 'wf', version: 3, input: {} }])
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('触发在飞时锁住版本选择,迟到错误仍对应原目标', async () => {
    const user = userEvent.setup()
    let rejectTrigger!: (response: Response) => void
    const pending = new Promise<Response>((resolve) => { rejectTrigger = resolve })
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/definitions') return ok({ items: [...published, { ...published[0], id: '2', name: 'other' }] })
      if (path === '/api/v1/executions' && init?.method === 'POST') return pending
      return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf v3')
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(screen.getByTestId(TID.triggerSubmit)).toHaveTextContent('正在触发…')
    expect(screen.getByTestId(TID.triggerDefinition)).toBeDisabled()
    rejectTrigger(ok({ error: 'conflict' }, 409))
    expect(await screen.findByRole('alert')).toHaveTextContent('wf v3')
    await waitFor(() => expect(screen.getByTestId(TID.triggerDefinition)).toBeEnabled())
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'other v3')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('已发布行缺版本号时阻止触发,不以省略 version 降级', async () => {
    const user = userEvent.setup()
    const posted = vi.fn()
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/definitions') return ok({ items: [{ ...published[0], version: null }] })
      if (path === '/api/v1/executions' && init?.method === 'POST') posted()
      return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await chooseOption(user, await screen.findByTestId(TID.triggerDefinition), 'wf')
    expect(screen.getByText('该版本无有效版本号，请刷新后重新选择')).toBeInTheDocument()
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
    expect(posted).not.toHaveBeenCalled()
  })

  it('触发输入非法 JSON:原地报错不发请求', async () => {
    const user = userEvent.setup()
    let posted = 0
    stubRoute((url, init) => {
      const path = pathOf(url)
      if (path === '/api/v1/executions' && init?.method === 'POST') posted += 1
      if (path === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (path === '/api/v1/definitions') return ok({ items: published })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'wf v3')
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: 'not-json' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(await screen.findByRole('alert')).toHaveTextContent('输入不是合法 JSON')
    expect(posted).toBe(0)
  })

  it('触发输入顶层是数组:同样就地拦下', async () => {
    const user = userEvent.setup()
    listStub(() => ok({ items: [], page: { limit: 20, offset: 0, total: 0 } }))
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    await chooseOption(user, screen.getByTestId(TID.triggerDefinition), 'wf v3')
    fireEvent.change(screen.getByTestId(TID.triggerInput), { target: { value: '[]' } })
    await user.click(screen.getByTestId(TID.triggerSubmit))
    expect(await screen.findByRole('alert')).toHaveTextContent('顶层必须是 JSON 对象')
  })

  it('已发布 >20 条全量可达:21 条全进触发下拉(审计 F03:第 21 个此前不进下拉)', async () => {
    const user = userEvent.setup()
    // fake 服务端按请求 limit/offset 切片,limit 缺省回 20(= 服务端
    // parsePageParams 行为):旧实现(裸 GET)只能拿 20 条,本用例为红;
    // 翻页实现(limit=100)取全 21 条,绿。期望值 21 来自 envelope total。
    const store = Array.from({ length: 21 }, (_, i) => ({
      id: String(i + 1),
      name: `wf-${i + 1}`,
      version: 1,
      status: 'PUBLISHED',
      description: '',
      updated_at: '2026-10-04T05:00:00Z',
    }))
    stubRoute((url) => {
      const path = pathOf(url)
      if (path === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (path === '/api/v1/definitions') {
        const q = queryOf(url)
        const limit = q.has('limit') ? Number(q.get('limit')) : 20
        const offset = Number(q.get('offset') ?? 0)
        return ok({ items: store.slice(offset, offset + limit), page: { limit, offset, total: store.length } })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    // 面板头计数此前停在 20;数据面取全后即 21。
    expect(await screen.findByText('21 个已发布')).toBeInTheDocument()
    await user.click(screen.getByTestId(TID.triggerDefinition))
    // 第 21 个已发布工作流在下拉里,且有可点路径。
    await screen.findByRole('option', { name: 'wf-21 v1' })
    // 21 个工作流 + 占位项 = 弹层 22 个选项。
    expect(screen.getAllByRole('option')).toHaveLength(22)
    expect(screen.getByRole('option', { name: '选择已发布工作流…' })).toHaveAttribute('aria-disabled', 'true')
  })

  it('无已发布工作流:触发禁用并给出下一步', async () => {
    stubRoute((url) => {
      const path = pathOf(url)
      if (path === '/api/v1/executions') return ok({ items: [], page: { limit: 20, offset: 0, total: 0 } })
      if (path === '/api/v1/definitions') return ok({ items: [] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.triggerDefinition)
    expect(screen.getByTestId(TID.triggerSubmit)).toBeDisabled()
    expect(screen.getByText(/尚无已发布工作流/)).toBeInTheDocument()
    expect(screen.queryByText('先选一个已发布工作流')).not.toBeInTheDocument()
    // 无筛选且无数据:走「暂无执行」而不是筛选空态,筛选栏也不占位
    const empty = await screen.findByTestId(TID.execEmpty)
    expect(empty).toHaveTextContent('暂无执行')
    expect(screen.queryByText('筛选')).not.toBeInTheDocument()
  })

  it('无选中:右栏是引导空态(不猜数据)', async () => {
    listStub(() => ok({ items: [], page: { limit: 20, offset: 0, total: 0 } }))
    render(<ExecutionsView selectedId={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByText('未选择执行')).toBeInTheDocument()
    expect(screen.queryByTestId(TID.execDetail)).not.toBeInTheDocument()
  })

  it('列表 401 → onAuthExpired(统一登出)', async () => {
    const onAuthExpired = vi.fn()
    stubRoute((url) => {
      if (pathOf(url) === '/api/v1/executions') {
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
