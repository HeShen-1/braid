// ExecutionDetail 测试(T21.1 Lane B):纵向分层 —— 身份芯片(裸 ENUM 走
// data-state,可见文本中文)、时间带节点行(并行实例 + 失败原因就近)、
// 会签中间态与离开挂起态的剪枝、回滚/续跑/取消(取消要第二下确认)、
// 409 错误面、终态输出与审计链。fetch 全 stub 按 URL 路由。
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionDetail } from './ExecutionDetail.tsx'
import { tokenStore } from './api.ts'
import { TID, tid } from './ui/testids.ts'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubRoute(routes: (string | RegExp)[], responder: (url: string, init?: RequestInit) => Response) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    for (const r of routes) {
      if (typeof r === 'string' ? url === r : r.test(url)) return responder(url, init)
    }
    return ok({ error: 'unexpected_url', url })
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

/** 钉版定义(定义版本图的数据面)。 */
const DOC = {
  dsl: '1.0',
  name: 'wf',
  nodes: [
    { id: 'gate', type: 'approval', title: '门一', approvers: ['alice'], mode: 'any', timeout_hours: 24, on_timeout: 'fail' },
    { id: 'sign', type: 'approval', title: '会签', approvers: ['alice', 'bob'], mode: 'all', timeout_hours: 24, on_timeout: 'fail' },
  ],
  edges: [{ from: 'gate', to: 'sign' }],
}

const baseDetail = {
  id: '7',
  definition_id: '1',
  definition_name: 'wf',
  definition_version: 1,
  status: 'FAILED',
  trigger_type: 'API',
  triggered_by: 'author',
  error_code: null,
  error_message: null,
  created_at: '2026-10-04T06:00:00Z',
  started_at: '2026-10-04T06:00:01Z',
  finished_at: null,
  definition_hash: 'h',
  input: { docs: ['a'] },
  outputs: null,
  nodes: [],
  audit: { exec_date: '2026-10-04', trace_id: null },
}

function nodeRow(node_id: string, instance_no: number, status: string, over: Record<string, unknown> = {}) {
  return {
    id: `${node_id}-${instance_no}`,
    node_id,
    instance_no,
    type: 'tool',
    status,
    attempts: 1,
    max_attempts: 1,
    error_code: null,
    error_message: null,
    started_at: '2026-10-04T06:00:02Z',
    finished_at: null,
    ...over,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('ExecutionDetail(执行详情)', () => {
  it('查看实例证据是独立入口：默认不读节点，查看后不改变已有回滚选点', async () => {
    const user = userEvent.setup()
    const calls: string[] = []
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/nodes\//, /\/audit/], (url) => {
      calls.push(url)
      if (url === '/api/v1/executions/7') return ok({ ...baseDetail, nodes: [nodeRow('gate', 0, 'FAILED'), nodeRow('fan', 2, 'SUCCEEDED')] })
      if (url.includes('/nodes/fan/attempts')) return ok({ items: [] })
      if (url.includes('/nodes/fan?')) return ok({ node_id: 'fan', instance_no: 2, attempts: 0, attempts_base: 0, status: 'PENDING', input_snapshot: null, output: null })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.nodeRow('gate', 0))
    expect(calls.filter((url) => url.includes('/nodes/'))).toHaveLength(0)
    await user.click(screen.getByTestId(tid.nodeRow('gate', 0)))
    await user.click(screen.getByTestId(tid.nodeEvidence('fan', 2)))
    expect(await screen.findByTestId(TID.nodeEvidence)).toHaveTextContent('fan #2')
    expect(screen.getByTestId(TID.nodeEvidence)).toHaveFocus()
    expect(screen.getByTestId(tid.nodeRow('gate', 0))).toHaveAttribute('data-selected', 'true')
    expect(screen.getByTestId(tid.nodeRow('fan', 2))).toHaveAttribute('data-selected', 'false')
    expect(screen.getByTestId(TID.rollbackSubmit)).toHaveTextContent('回滚到 gate')
    expect(calls).toContain('/api/v1/executions/7/nodes/fan?instance_no=2')
    await user.click(screen.getByRole('button', { name: '收起证据' }))
    expect(screen.getByTestId(tid.nodeEvidence('fan', 2))).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(await screen.findByTestId(TID.nodeEvidence)).toHaveFocus()
  })

  it('Trace鼠标/键盘下钻定位面板，收起回到真实入口', async () => {
    const user = userEvent.setup()
    const trace = 'a'.repeat(32)
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/audit/], (url) => url === '/api/v1/executions/7' ? ok({ ...baseDetail, audit: { ...baseDetail.audit, trace_id: trace } }) : ok({ items: [], next_cursor: '' }))
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    const opener = await screen.findByRole('button', { name: `查询执行 trace ${trace}` })
    await user.click(opener)
    expect(await screen.findByRole('region', { name: 'Trace 审计' })).toHaveFocus()
    await user.click(screen.getByRole('button', { name: '收起 Trace' }))
    expect(opener).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(await screen.findByRole('region', { name: 'Trace 审计' })).toHaveFocus()
  })

  it('Trace内重查同链或跨链A→B，收起仍返回有效外部入口', async () => {
    const user = userEvent.setup()
    const a = 'a'.repeat(32), b = 'b'.repeat(32)
    const ev = (id: string, trace_id: string) => ({ id, event_type: 'execution.triggered', node_id: null, trace_id, payload: {}, row_hash: null, prev_hash: '', actor_type: 'user', actor_id: 'author', event_at: baseDetail.created_at })
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/audit/], (url) => {
      if (url === '/api/v1/executions/7') return ok({ ...baseDetail, audit: { ...baseDetail.audit, trace_id: a } })
      return ok({ items: url.includes(`trace_id=${b}`) ? [ev('2', b)] : [ev('1', a), ev('2', b)], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    const external = await screen.findByRole('button', { name: `查询执行 trace ${a}` })
    await user.click(external)
    let panel = await screen.findByRole('region', { name: 'Trace 审计' })
    await user.click(await within(panel).findByRole('button', { name: `查询 trace ${a}` }))
    await user.click(within(panel).getByRole('button', { name: '收起 Trace' }))
    expect(external).toHaveFocus()
    await user.keyboard('{Enter}')
    panel = await screen.findByRole('region', { name: 'Trace 审计' })
    const internal = await within(panel).findByRole('button', { name: `查询 trace ${b}` })
    await user.click(internal)
    panel = await screen.findByRole('region', { name: 'Trace 审计' })
    await within(panel).findByRole('button', { name: `查询 trace ${b}` })
    expect(internal.isConnected).toBe(false)
    await user.click(within(panel).getByRole('button', { name: '收起 Trace' }))
    expect(external).toHaveFocus()
  })

  it('加载态就给 exec-detail(不带假状态);数据到达后行级 data-state + 失败原因可见', async () => {
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/audit/], (url) => {
      if (url === '/api/v1/executions/7') {
        return ok({
          ...baseDetail,
          nodes: [
            nodeRow('fan', 0, 'SUCCEEDED', { finished_at: '2026-10-04T06:00:05Z' }),
            nodeRow('fan', 1, 'FAILED', { finished_at: '2026-10-04T06:00:04Z' }),
            nodeRow('gate', 0, 'FAILED', { type: 'approval', error_code: 'approval_rejected' }),
          ],
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    expect(screen.getByTestId(TID.execDetail)).toBeInTheDocument()
    expect(screen.queryByTestId(TID.execStatus)).not.toBeInTheDocument()

    expect(await screen.findByTestId(tid.nodeRow('fan', 0))).toHaveAttribute('data-state', 'SUCCEEDED')
    expect(screen.getByTestId(tid.nodeRow('fan', 1))).toHaveAttribute('data-state', 'FAILED')
    expect(screen.getByTestId(tid.nodeRow('fan', 0))).toHaveTextContent('已成功')
    expect(screen.getByTestId(tid.nodeRow('gate', 0))).toHaveTextContent('approval_rejected')
    // 并行实例各自成行(instance_no 进 testid 与标签列)
    expect(screen.getByTestId(tid.nodeRow('fan', 0))).toHaveTextContent('#0')
    expect(screen.getByTestId(tid.nodeRow('fan', 1))).toHaveTextContent('#1')
  })

  it('执行状态芯片:data-state = 原始 ENUM,可见文本 = 中文标签', async () => {
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/audit/], (url) => {
      if (url === '/api/v1/executions/7') return ok(baseDetail)
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    const chip = await screen.findByTestId(TID.execStatus)
    expect(chip).toHaveAttribute('data-state', 'FAILED')
    expect(chip).toHaveTextContent('已失败')
  })

  it('会签中间态:待投名单 + 决定时间线;离开挂起态后子面板剪枝', async () => {
    const user = userEvent.setup()
    let waiting = true
    stubRoute([/^\/api\/v1\/executions\/7$/, /approvals\/sign$/, /\/audit/], (url) => {
      if (url.endsWith('/approvals/sign')) {
        return ok({
          execution_id: '7',
          node_id: 'sign',
          node_status: 'WAITING_APPROVAL',
          deadline_at: null,
          approvers: ['alice', 'bob'],
          mode: 'all',
          pending_approvers: ['alice', 'bob'],
          decisions: [],
        })
      }
      if (url === '/api/v1/executions/7') {
        return ok({
          ...baseDetail,
          status: waiting ? 'WAITING_APPROVAL' : 'SUCCEEDED',
          nodes: [nodeRow('sign', 0, waiting ? 'WAITING_APPROVAL' : 'SUCCEEDED', { type: 'approval' })],
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    const panel = await screen.findByTestId(tid.approvalEntry('sign'))
    expect(panel).toHaveTextContent('会签')
    expect(panel).toHaveTextContent('alice')
    expect(screen.getByTestId(TID.pendingApprovers).textContent).toBe('alice, bob')

    waiting = false
    await user.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(screen.queryByTestId(tid.approvalEntry('sign'))).not.toBeInTheDocument())
  })

  it('回滚:点时间带行选目标 → POST rollback(体带 node_id)→ flash 回显 reset_count', async () => {
    const user = userEvent.setup()
    let rolled = false
    stubRoute([/^\/api\/v1\/executions\/7$/, /rollback$/, /\/audit/], (url, init) => {
      if (url.endsWith('/rollback')) {
        rolled = true
        expect(init?.body).toBe(JSON.stringify({ node_id: 'gate' }))
        return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 2, quiesced_count: 0 }, 201)
      }
      if (url === '/api/v1/executions/7') {
        return ok({
          ...baseDetail,
          status: rolled ? 'RUNNING' : 'FAILED',
          nodes: [nodeRow('gate', 0, rolled ? 'WAITING_APPROVAL' : 'FAILED')],
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    const row = await screen.findByTestId(tid.nodeRow('gate', 0))
    expect(screen.getByTestId(TID.rollbackSubmit)).toBeDisabled()
    expect(screen.getByTestId(TID.rollbackSubmit)).toHaveTextContent('先选节点')

    await user.click(row)
    expect(row).toHaveAttribute('data-selected', 'true')
    expect(screen.getByTestId(TID.rollbackSubmit)).toBeEnabled()
    await user.click(screen.getByTestId(TID.rollbackSubmit))
    await user.click(screen.getByTestId(TID.rollbackConfirm))
    expect(await screen.findByTestId(TID.execFlash)).toHaveTextContent('重置 2 行')
    expect(rolled).toBe(true)
  })

  it('F10 回滚确认层:单击只布防(指名目标),Esc 收起焦点回按钮,确认后才发请求', async () => {
    const user = userEvent.setup()
    let fired = false
    stubRoute([/^\/api\/v1\/executions\/7$/, /rollback$/, /\/audit/], (url) => {
      if (url.endsWith('/rollback')) {
        fired = true
        return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 1, quiesced_count: 0 }, 201)
      }
      if (url === '/api/v1/executions/7') return ok({ ...baseDetail, nodes: [nodeRow('gate', 0, 'FAILED')] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await user.click(await screen.findByTestId(tid.nodeRow('gate', 0)))
    // 单击只布防:请求未发,确认层在场且指名目标节点
    await user.click(screen.getByTestId(TID.rollbackSubmit))
    expect(fired).toBe(false)
    const layer = screen.getByRole('group', { name: '确认回滚到 gate' })
    expect(layer).toHaveTextContent('gate')
    // Esc 收起,焦点回回滚按钮(同「取消执行」契约)
    await user.keyboard('{Escape}')
    expect(screen.queryByTestId(TID.rollbackConfirm)).toBeNull()
    expect(screen.getByTestId(TID.rollbackSubmit)).toHaveFocus()
    // 再布防 → 确认 → 请求才发出
    await user.click(screen.getByTestId(TID.rollbackSubmit))
    await user.click(screen.getByTestId(TID.rollbackConfirm))
    expect(await screen.findByTestId(TID.execFlash)).toBeInTheDocument()
    expect(fired).toBe(true)
  })

  it('T19.6 quiesce:勾选后回滚体带旗标,flash 带静默行数', async () => {
    const user = userEvent.setup()
    let sawQuiesce = false
    stubRoute([/^\/api\/v1\/executions\/7$/, /rollback$/, /\/audit/], (url, init) => {
      if (url.endsWith('/rollback')) {
        expect(init?.body).toBe(JSON.stringify({ node_id: 'gate', quiesce: true }))
        sawQuiesce = true
        return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 1, quiesced_count: 1 }, 201)
      }
      if (url === '/api/v1/executions/7') {
        return ok({ ...baseDetail, nodes: [nodeRow('gate', 0, 'FAILED')] })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await user.click(await screen.findByTestId(tid.nodeRow('gate', 0)))
    // 决策 3 收敛后 Checkbox 由 Base UI 承载:label 同时关联隐藏 input 与
    // role=checkbox 的可见控件(label 断言保留,故按 role + 可及名取)。
    await user.click(screen.getByRole('checkbox', { name: '静默在飞' }))
    await user.click(screen.getByTestId(TID.rollbackSubmit))
    await user.click(screen.getByTestId(TID.rollbackConfirm))
    expect(await screen.findByTestId(TID.execFlash)).toHaveTextContent('静默在飞 1 行')
    expect(sawQuiesce).toBe(true)
  })

  it('409 blocked_inflight:detail 与 blocked 清单进错误面', async () => {
    const user = userEvent.setup()
    stubRoute([/^\/api\/v1\/executions\/7$/, /rollback$/, /\/audit/], (url) => {
      if (url.endsWith('/rollback')) {
        return new Response(
          JSON.stringify({ error: 'rollback_blocked_inflight', detail: 'inflight residue', blocked: ['fan:2', 'sign:0'] }),
          { status: 409, headers: { 'Content-Type': 'application/json' } },
        )
      }
      if (url === '/api/v1/executions/7') return ok({ ...baseDetail, nodes: [nodeRow('gate', 0, 'FAILED')] })
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await user.click(await screen.findByTestId(tid.nodeRow('gate', 0)))
    await user.click(screen.getByTestId(TID.rollbackSubmit))
    await user.click(screen.getByTestId(TID.rollbackConfirm))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('rollback_blocked_inflight')
    expect(alert).toHaveTextContent('inflight residue')
    expect(alert).toHaveTextContent('fan:2')
    expect(alert).toHaveTextContent('sign:0')
  })

  it('SUCCEEDED:续跑可用、回滚需选点、取消禁用;输出与审计链逐行可数;续跑无自动点 → 服务端错误面', async () => {
    const user = userEvent.setup()
    stubRoute([/^\/api\/v1\/executions\/9$/, /resume$/, /\/audit/], (url) => {
      if (url.endsWith('/resume')) {
        return new Response(JSON.stringify({ error: 'no_resume_target' }), {
          status: 400,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      if (url === '/api/v1/executions/9') {
        return ok({ ...baseDetail, id: '9', status: 'SUCCEEDED', outputs: { report: 'approved' } })
      }
      return ok({
        items: [
          { id: '30', exec_date: '2026-10-04', event_at: '2026-10-04T06:00:00Z', created_at: '2026-10-04T06:00:00Z', event_type: 'execution.triggered', execution_id: '9', node_execution_id: null, node_id: null, attempt_id: null, actor_type: 'user', actor_id: 'author', trace_id: null, pii_masked: false, prev_hash: '', row_hash: 'abcdef1234567890', payload: {} },
          { id: '31', exec_date: '2026-10-04', event_at: '2026-10-04T06:01:00Z', created_at: '2026-10-04T06:01:00Z', event_type: 'execution.rolled_back', execution_id: '9', node_execution_id: null, node_id: 'gate', attempt_id: null, actor_type: 'user', actor_id: 'author', trace_id: null, pii_masked: false, prev_hash: 'abcdef1234567890', row_hash: 'ffeeddccbbaa9988', payload: { reset_count: 2 } },
        ],
        next_cursor: '',
      })
    })
    render(<ExecutionDetail execId="9" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.resumeSubmit)).toBeEnabled()
    expect(screen.getByTestId(TID.rollbackSubmit)).toBeDisabled()
    expect(screen.getByTestId(TID.rollbackSubmit)).toHaveTextContent('先选节点')
    // 终态不可取消:外层「取消执行」禁用(确认条不展开,故 cancel-submit 不在场)
    expect(screen.getByRole('button', { name: '取消执行' })).toBeDisabled()
    expect(screen.queryByTestId(TID.cancelSubmit)).not.toBeInTheDocument()
    expect(screen.getByTestId(TID.execOutputs)).toHaveTextContent('approved')

    const audit = screen.getByTestId(TID.auditList)
    expect(await within(audit).findByTestId(tid.auditRow('execution.triggered'))).toHaveTextContent('abcdef12')
    expect(await within(audit).findByTestId(tid.auditRow('execution.rolled_back'))).toHaveTextContent('gate')

    await user.click(screen.getByTestId(TID.resumeSubmit))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('no_resume_target')
  })

  it('RUNNING 取消:第一下只是展开确认条(不发请求),第二下才 POST cancel;Esc 收起', async () => {
    const user = userEvent.setup()
    let cancelled = false
    let posts = 0
    stubRoute([/^\/api\/v1\/executions\/8$/, /cancel$/, /\/audit/], (url, init) => {
      if (url.endsWith('/cancel')) {
        posts += 1
        cancelled = true
        expect(init?.body).toBeUndefined()
        return ok({ execution_id: '8', from_status: 'RUNNING', cancelled_count: 3 })
      }
      if (url === '/api/v1/executions/8') {
        return ok({
          ...baseDetail,
          id: '8',
          status: cancelled ? 'CANCELLED' : 'RUNNING',
          nodes: [nodeRow('a', 0, cancelled ? 'CANCELLED' : 'EXECUTING', { type: 'llm', finished_at: null })],
        })
      }
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="8" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(tid.nodeRow('a', 0))).toHaveAttribute('data-state', 'EXECUTING')
    expect(screen.queryByTestId(TID.cancelSubmit)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '取消执行' }))
    expect(posts).toBe(0)
    expect(screen.getByTestId(TID.cancelSubmit)).toBeInTheDocument()

    // Esc 收起确认条(浮层契约),再打开才真提交
    fireEvent.keyDown(screen.getByRole('group', { name: '确认取消执行' }), { key: 'Escape' })
    await waitFor(() => expect(screen.queryByTestId(TID.cancelSubmit)).not.toBeInTheDocument())
    // T21.4 增项:收起后焦点回入口按钮(原先掉回 body,键盘用户要重新 Tab 一遍)
    expect(screen.getByRole('button', { name: '取消执行' })).toHaveFocus()
    await user.click(screen.getByRole('button', { name: '取消执行' }))
    await user.click(screen.getByTestId(TID.cancelSubmit))

    expect(await screen.findByTestId(TID.execFlash)).toHaveTextContent('楔停 3 行')
    expect(posts).toBe(1)
    expect(screen.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'CANCELLED')
  })

  it('无节点记录:时间带给空态而不是空轨道', async () => {
    stubRoute([/^\/api\/v1\/executions\/7$/, /\/audit/], (url) => {
      if (url === '/api/v1/executions/7') return ok(baseDetail)
      return ok({ items: [], next_cursor: '' })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.execRibbon)).toHaveTextContent('暂无节点执行记录')
  })

  it('定义版本图:默认收起零请求;展开才拉钉版定义,并把执行状态注入图上', async () => {
    const user = userEvent.setup()
    const fetchMock = stubRoute(
      [/^\/api\/v1\/executions\/7$/, /\/definitions\/wf\/versions\/1$/, /\/audit/],
      (url) => {
        if (url === '/api/v1/executions/7') {
          return ok({
            ...baseDetail,
            nodes: [nodeRow('gate', 0, 'FAILED', { type: 'approval', error_code: 'approval_rejected' })],
          })
        }
        if (url.endsWith('/versions/1')) {
          return ok({ id: '1', name: 'wf', version: 1, status: 'PUBLISHED', description: '', updated_at: '2026-10-04T05:00:00Z', dsl: DOC })
        }
        return ok({ items: [], next_cursor: '' })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.nodeRow('gate', 0))
    // 收起:标题带 name vX,且没有版本请求
    expect(screen.getByText('wf v1')).toBeInTheDocument()
    const versionCalls = () => fetchMock.mock.calls.filter(([u]) => String(u).includes('/versions/')).length
    expect(versionCalls()).toBe(0)

    await user.click(screen.getByRole('button', { name: '展开定义版本图' }))
    // 状态注入:定义里的 gate 节点直接显示当前执行的聚合状态
    expect(await screen.findByTestId(tid.nodeCard('gate'))).toHaveAttribute('data-state', 'FAILED')
    expect(screen.getByTestId(tid.nodeCard('sign'))).not.toHaveAttribute('data-state')
    expect(versionCalls()).toBe(1)
    // 时间带的实例级锚不被图复制
    expect(screen.getAllByTestId(tid.nodeRow('gate', 0))).toHaveLength(1)
  })
})
