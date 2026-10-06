// ExecutionDetail 组件测试(T16.6):节点表(并行实例行)、会签中间态、
// 回滚/续跑操作面(含 409 错误面)、审计区、终态 outputs。fetch 全 stub,
// 按 URL 路由(同 App.test.tsx 习语)。
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionDetail } from './ExecutionDetail.tsx'
import { tokenStore } from './api.ts'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubRoute(routes: (string | RegExp)[], responder: (url: string, init?: RequestInit) => Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      for (const r of routes) {
        if (typeof r === 'string' ? url === r : r.test(url)) return responder(url, init)
      }
      return ok({ error: 'unexpected_url', url })
    }),
  )
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
  audit: { exec_date: '2026-10-04', trace_id: null },
}

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('ExecutionDetail(执行详情)', () => {
  it('节点表全字段渲染:并行实例行独立成行', async () => {
    stubRoute(['/api/v1/executions/7', /versions/], (url) => {
      if (url === '/api/v1/executions/7') {
        return ok({
          ...baseDetail,
          nodes: [
            { id: '10', node_id: 'fan', instance_no: 0, type: 'parallel', status: 'SUCCEEDED', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: '2026-10-04T06:00:05Z' },
            { id: '11', node_id: 'fan', instance_no: 1, type: 'tool', status: 'SUCCEEDED', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: '2026-10-04T06:00:04Z' },
            { id: '12', node_id: 'fan', instance_no: 2, type: 'tool', status: 'FAILED', attempts: 1, max_attempts: 1, error_code: 'bad_tool_output', error_message: 'x', started_at: null, finished_at: '2026-10-04T06:00:04Z' },
            { id: '13', node_id: 'gate', instance_no: 0, type: 'approval', status: 'FAILED', attempts: 1, max_attempts: 1, error_code: 'approval_rejected', error_message: null, started_at: null, finished_at: '2026-10-04T06:00:03Z' },
          ],
        })
      }
      return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
    })
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId('node-row-fan-0')).toBeInTheDocument()
    expect(screen.getByTestId('node-row-fan-1')).toBeInTheDocument()
    expect(screen.getByTestId('node-row-fan-2')).toBeInTheDocument()
    expect(screen.getByTestId('node-row-gate-0')).toHaveTextContent('approval_rejected')
  })

  it('会签中间态:WAITING_APPROVAL 审批行拉子资源,渲染待投名单与决定时间线', async () => {
    stubRoute(
      ['/api/v1/executions/7', /approvals\/sign$/, /versions/, /audit/],
      (url) => {
        if (url.endsWith('/approvals/sign')) {
          return ok({
            execution_id: '7', node_id: 'sign', node_status: 'WAITING_APPROVAL', deadline_at: null,
            approvers: ['alice', 'bob'], mode: 'all', pending_approvers: ['bob'],
            decisions: [{ id: '20', execution_id: '7', node_id: 'sign', round: 1, decision: 'approve', decided_by: 'alice', comment: null, decided_at: '2026-10-04T06:01:00Z' }],
          })
        }
        if (url === '/api/v1/executions/7') {
          return ok({
            ...baseDetail, status: 'WAITING_APPROVAL',
            nodes: [{ id: '13', node_id: 'sign', instance_no: 0, type: 'approval', status: 'WAITING_APPROVAL', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: null }],
          })
        }
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId('approval-sign')).toBeInTheDocument()
    expect(screen.getByTestId('pending-approvers')).toHaveTextContent('bob')
    expect(screen.getByTestId('approval-sign')).toHaveTextContent('会签')
    expect(screen.getByTestId('approval-sign')).toHaveTextContent('alice')
  })

  it('回滚:选节点 → POST rollback → 成功 flash 带 reset_count', async () => {
    const user = userEvent.setup()
    let rolled = false
    stubRoute(
      [/^\/api\/v1\/executions\/7$/, /rollback$/, /resume$/, /versions/, /audit/, /approvals\//],
      (url, init) => {
        if (url.endsWith('/rollback')) {
          rolled = true
          expect(init?.body).toBe(JSON.stringify({ node_id: 'gate' }))
          return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 2, quiesced_count: 0 }, 201)
        }
        if (url === '/api/v1/executions/7') {
          return ok({
            ...baseDetail, status: rolled ? 'RUNNING' : 'FAILED',
            nodes: [{ id: '13', node_id: 'gate', instance_no: 0, type: 'approval', status: rolled ? 'EXECUTING' : 'FAILED', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: null }],
          })
        }
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId('node-row-gate-0')
    await user.click(screen.getByTestId('node-row-gate-0'))
    await user.click(screen.getByTestId('rollback-submit'))
    expect(await screen.findByTestId('exec-flash')).toHaveTextContent('2')
    expect(rolled).toBe(true)
  })

  it('T19.6 quiesce:勾选后回滚体带旗标,flash 带静默行数', async () => {
    const user = userEvent.setup()
    let rolled = false
    stubRoute(
      [/^\/api\/v1\/executions\/7$/, /rollback$/, /versions/, /audit/, /approvals\//],
      (url, init) => {
        if (url.endsWith('/rollback')) {
          rolled = true
          expect(init?.body).toBe(JSON.stringify({ node_id: 'gate', quiesce: true }))
          return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 1, quiesced_count: 1 }, 201)
        }
        if (url === '/api/v1/executions/7') {
          return ok({
            ...baseDetail, status: rolled ? 'RUNNING' : 'FAILED',
            nodes: [{ id: '13', node_id: 'gate', instance_no: 0, type: 'approval', status: rolled ? 'EXECUTING' : 'FAILED', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: null }],
          })
        }
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId('node-row-gate-0')
    await user.click(screen.getByTestId('node-row-gate-0'))
    // 勾选旗标 → 体携 quiesce;缺省路径由上一用例锚(体形状不变)。
    await user.click(screen.getByTestId('quiesce-toggle'))
    await user.click(screen.getByTestId('rollback-submit'))
    expect(await screen.findByTestId('exec-flash')).toHaveTextContent('静默在飞 1 行')
    expect(rolled).toBe(true)
  })

  it('续跑:POST resume 无体', async () => {
    const user = userEvent.setup()
    let resumed = false
    stubRoute(
      [/^\/api\/v1\/executions\/7$/, /resume$/, /versions/, /audit/, /approvals\//],
      (url, init) => {
        if (url.endsWith('/resume')) {
          resumed = true
          expect(init?.body).toBeUndefined()
          return ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 1 }, 201)
        }
        if (url === '/api/v1/executions/7') {
          return ok({ ...baseDetail, status: resumed ? 'RUNNING' : 'FAILED', nodes: [] })
        }
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId('resume-submit')
    await user.click(screen.getByTestId('resume-submit'))
    expect(await screen.findByTestId('exec-flash')).toBeInTheDocument()
    expect(resumed).toBe(true)
  })

  it('409 blocked_inflight:detail 与 blocked 清单进错误面', async () => {
    const user = userEvent.setup()
    stubRoute(
      [/^\/api\/v1\/executions\/7$/, /rollback$/, /versions/, /audit/, /approvals\//],
      (url) => {
        if (url.endsWith('/rollback')) {
          return new Response(
            JSON.stringify({ error: 'rollback_blocked_inflight', detail: 'inflight residue', blocked: ['fan:2', 'sign:0'] }),
            { status: 409, headers: { 'Content-Type': 'application/json' } },
          )
        }
        if (url === '/api/v1/executions/7') return ok({ ...baseDetail, nodes: [{ id: '13', node_id: 'gate', instance_no: 0, type: 'approval', status: 'FAILED', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: null, finished_at: null }] })
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="7" onAuthExpired={() => {}} />)
    await screen.findByTestId('node-row-gate-0')
    await user.click(screen.getByTestId('node-row-gate-0'))
    await user.click(screen.getByTestId('rollback-submit'))
    const err = await screen.findByTestId('exec-error')
    expect(err).toHaveTextContent('rollback_blocked_inflight')
    expect(err).toHaveTextContent('inflight residue')
    expect(err).toHaveTextContent('fan:2')
    expect(err).toHaveTextContent('sign:0')
  })

  it('SUCCEEDED 态(T19.3):回滚/续跑可用、取消禁用;续跑无自动点 → 服务端 no_resume_target 错误面渲染', async () => {
    const user = userEvent.setup()
    stubRoute(
      [/^\/api\/v1\/executions\/9$/, /resume$/, /versions/, /audit/, /approvals\//],
      (url) => {
        if (url.endsWith('/resume')) {
          return new Response(JSON.stringify({ error: 'no_resume_target' }), {
            status: 400,
            headers: { 'Content-Type': 'application/json' },
          })
        }
        if (url === '/api/v1/executions/9') {
          return ok({ ...baseDetail, id: '9', status: 'SUCCEEDED', outputs: { report: 'ok' }, nodes: [] })
        }
        if (url.includes('/audit')) {
          return ok({
            items: [
              { id: '30', exec_date: '2026-10-04', event_at: '2026-10-04T06:00:00Z', created_at: '2026-10-04T06:00:00Z', event_type: 'execution.triggered', execution_id: '9', node_execution_id: null, node_id: null, attempt_id: null, actor_type: 'user', actor_id: 'author', trace_id: null, pii_masked: false, prev_hash: '', row_hash: 'h', payload: {} },
              { id: '31', exec_date: '2026-10-04', event_at: '2026-10-04T06:01:00Z', created_at: '2026-10-04T06:01:00Z', event_type: 'execution.rolled_back', execution_id: '9', node_execution_id: null, node_id: 'gate', attempt_id: null, actor_type: 'user', actor_id: 'author', trace_id: null, pii_masked: false, prev_hash: 'h', row_hash: 'h2', payload: { reset_count: 2, rerun_target: true } },
            ],
            next_cursor: '',
          })
        }
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="9" onAuthExpired={() => {}} />)
    // 回滚按钮在 SUCCEEDED 态可用性放开,但 node 选择前置(nodes 空集 ⇒
    // disabled + 引导文案);resume 无选择前置,直接可用。
    expect(await screen.findByTestId('resume-submit')).toBeEnabled()
    expect(screen.getByTestId('rollback-submit')).toBeDisabled()
    expect(screen.getByTestId('rollback-submit')).toHaveTextContent('先选节点')
    expect(screen.getByTestId('cancel-submit')).toBeDisabled()
    expect(screen.getByTestId('exec-outputs')).toHaveTextContent('report')
    expect(await screen.findByTestId('audit-row-execution.triggered')).toBeInTheDocument()
    expect(screen.getByTestId('audit-row-execution.rolled_back')).toBeInTheDocument()
    // SUCCEEDED 无自动定位点:续跑点击 → 400 no_resume_target 进错误面
    //(服务端分面语义,前端如实呈现)。
    await user.click(screen.getByTestId('resume-submit'))
    const err = await screen.findByTestId('exec-error')
    expect(err).toHaveTextContent('no_resume_target')
  })

  it('RUNNING 态(T19.3):取消可用 → POST cancel → flash 回显楔停行数;回滚/续跑禁用', async () => {
    const user = userEvent.setup()
    let cancelled = false
    stubRoute(
      [/^\/api\/v1\/executions\/8$/, /cancel$/, /versions/, /audit/, /approvals\//],
      (url, init) => {
        if (url.endsWith('/cancel')) {
          cancelled = true
          expect(init?.body).toBeUndefined()
          return ok({ execution_id: '8', from_status: 'RUNNING', cancelled_count: 3 })
        }
        if (url === '/api/v1/executions/8') {
          return ok({
            ...baseDetail, id: '8', status: cancelled ? 'CANCELLED' : 'RUNNING',
            nodes: [{ id: '14', node_id: 'a', instance_no: 0, type: 'llm', status: cancelled ? 'CANCELLED' : 'EXECUTING', attempts: 1, max_attempts: 1, error_code: null, error_message: null, started_at: '2026-10-05T06:00:01Z', finished_at: null }],
          })
        }
        if (url.includes('/audit')) return ok({ items: [], next_cursor: '' })
        return ok({ id: '1', dsl: { dsl: '1.0', name: 'wf', nodes: [], edges: [] } })
      },
    )
    render(<ExecutionDetail execId="8" onAuthExpired={() => {}} />)
    expect(await screen.findByTestId('cancel-submit')).toBeEnabled()
    expect(screen.getByTestId('rollback-submit')).toBeDisabled()
    expect(screen.getByTestId('resume-submit')).toBeDisabled()
    await user.click(screen.getByTestId('cancel-submit'))
    expect(await screen.findByTestId('exec-flash')).toHaveTextContent('3')
    expect(cancelled).toBe(true)
    expect(screen.getByTestId('exec-status')).toHaveTextContent('CANCELLED')
  })
})
