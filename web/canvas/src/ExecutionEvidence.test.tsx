import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuditEvidence, NodeEvidence } from './ExecutionEvidence.tsx'
import type { AuditEvent, NodeAttempt, NodeProjection } from './api.ts'
import { TID, tid } from './ui/testids.ts'

const noop = () => {}
const ok = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status })
const attempt: NodeAttempt = {
  attempt_id: '9007199254740993', round: 7, status: 'SUCCEEDED', worker_id: null,
  model: 'model-real', prompt_tokens: 12, completion_tokens: 3, total_tokens: 15,
  cost_amount: 1.25, cost_currency: 'CNY', estimated: true,
  error_code: null, error_message: null, started_at: '2026-10-16T00:00:00Z', finished_at: '2026-10-16T00:00:02Z',
  input_snapshot: { text: 'round-seven-input' }, output: { text: 'round-seven-output' },
}
const projection: NodeProjection = {
  execution_id: '7', node_execution_id: '8', node_id: 'fan', instance_no: 2, type: 'llm', status: 'SUCCEEDED',
  attempts: 7, attempts_base: 6, max_attempts: 2, input_snapshot: { text: 'projection-input' }, output: { text: 'projection-output' },
  error_code: null, error_message: null, started_at: '2026-10-15T00:00:00Z', finished_at: '2026-10-16T00:00:02Z',
}
function audit(i: number, over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: String(i), exec_date: '2026-10-16', execution_id: '7', event_at: '2026-10-16T00:00:00Z', created_at: '2026-10-16T00:00:01Z',
    event_type: 'llm.call.completed', node_execution_id: '8', node_id: 'fan', attempt_id: attempt.attempt_id,
    actor_type: 'worker', actor_id: 'w', trace_id: 'a'.repeat(32), pii_masked: false, row_hash: 'b'.repeat(64), prev_hash: 'c'.repeat(64),
    model: 'model-real', prompt_tokens: 12, completion_tokens: 3, total_tokens: 15, cost_amount: 1.25, cost_currency: 'CNY', cost_estimated: true,
    payload: { proof: 'actual-payload' }, ...over,
  }
}

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers() })

describe('节点投影与跨轮台账', () => {
  it('3500ms慢节点读跨2500ms轮询仍完成；超出投影attempts的轮次不猜归属', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => new Promise<Response>((resolve) => {
      const url = String(input)
      setTimeout(() => resolve(ok(url.includes('/attempts/8?') ? { ...attempt, round: 8, attempt_id: 'next' } : url.includes('/attempts?') ? { items: [attempt, { ...attempt, round: 8, attempt_id: 'next' }] } : projection)), 3500)
    })))
    const draw = (refreshKey: number) => <NodeEvidence execId="7" nodeId="fan" instanceNo={2} refreshKey={refreshKey} onAuthExpired={noop} onClose={noop} />
    const view = render(draw(1))
    await act(() => vi.advanceTimersByTimeAsync(2500))
    view.rerender(draw(2))
    await act(() => vi.advanceTimersByTimeAsync(1000))
    expect(screen.getByTestId(tid.attemptRow(8))).toHaveTextContent('投影尚未同步')
    expect(screen.getByTestId(tid.attemptRow(8))).not.toHaveTextContent('当前轮代')
    await act(() => vi.advanceTimersByTimeAsync(2500))
    view.rerender(draw(3))
    await act(() => vi.advanceTimersByTimeAsync(1000))
    expect(screen.getByTestId(TID.attemptEvidence)).toHaveTextContent('round-seven-output')
  })

  it('实例2按需读三端点，历史轮次/当前轮代保留，逐轮四件套与真实成本可读', async () => {
    const user = userEvent.setup()
    const calls: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input); calls.push(url)
      if (url.includes('/attempts/6?')) return ok({ ...attempt, round: 6, input_snapshot: { text: 'round-six-input' }, output: null, status: 'DEAD', finished_at: null, cost_amount: null, cost_currency: null, prompt_tokens: null, completion_tokens: null, total_tokens: null })
      if (url.includes('/attempts/7?')) return ok(attempt)
      if (url.includes('/attempts?')) return ok({ items: [{ ...attempt, round: 6, status: 'DEAD', finished_at: null, cost_amount: null, cost_currency: null, prompt_tokens: null, completion_tokens: null, total_tokens: null }, attempt] })
      return ok(projection)
    }))
    render(<NodeEvidence execId="7" nodeId="fan" instanceNo={2} refreshKey={1} onAuthExpired={noop} onClose={noop} />)
    const evidence = await screen.findByTestId(TID.attemptEvidence)
    await waitFor(() => expect(evidence).toHaveTextContent('round-seven-output'))
    expect(evidence).toHaveTextContent('输入 12 · 输出 3 · 总计 15 tokens')
    expect(evidence).toHaveTextContent('成本 1.25 CNY · 估算')
    expect(evidence).toHaveTextContent('2.0s')
    expect(screen.getByTestId(tid.attemptRow(7))).toHaveTextContent('当前轮代')
    expect(screen.getByTestId(tid.attemptRow(6))).toHaveTextContent('历史轮次')
    await user.click(screen.getByRole('button', { name: '第 6 轮' }))
    await waitFor(() => expect(evidence).toHaveTextContent('round-six-input'))
    expect(evidence).toHaveTextContent('未知（未记录结束时刻）')
    expect(evidence).toHaveTextContent('成本 未记录')
    expect(evidence).not.toHaveTextContent('成本 0')
    expect(evidence).not.toHaveTextContent('round-seven-output')
    expect(calls.every((url) => url.endsWith('instance_no=2'))).toBe(true)
    expect(calls).toContain('/api/v1/executions/7/nodes/fan/attempts/7?instance_no=2')
  })

  it('保留期单轮404不是零成本或虚构空输出，可刷新重试', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).includes('/attempts/7?') ? ok({ error: 'not_found' }, 404) : String(input).includes('/attempts?') ? ok({ items: [attempt] }) : ok(projection)))
    render(<NodeEvidence execId="7" nodeId="fan" instanceNo={2} refreshKey={1} onAuthExpired={noop} onClose={noop} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('可能未物化、已清理或超出范围')
    expect(screen.getByTestId(TID.attemptEvidence)).not.toHaveTextContent('round-seven-output')
    expect(screen.getByRole('button', { name: '刷新证据' })).toBeEnabled()
  })

  it('切换实例后迟到的旧响应不污染新证据，切轮后旧单轮响应也不回写', async () => {
    const user = userEvent.setup()
    let resolveOld!: (r: Response) => void
    let resolveRound!: (r: Response) => void
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('instance_no=1')) return new Promise<Response>((resolve) => { resolveOld = resolve })
      if (url.includes('/attempts/7?')) return new Promise<Response>((resolve) => { resolveRound = resolve })
      if (url.includes('/attempts/6?')) return ok({ ...attempt, round: 6, output: 'round-six' })
      return url.includes('/attempts?') ? ok({ items: [{ ...attempt, round: 6 }, attempt] }) : ok(projection)
    }))
    const view = render(<NodeEvidence key="one" execId="7" nodeId="fan" instanceNo={1} refreshKey={1} onAuthExpired={noop} onClose={noop} />)
    view.rerender(<NodeEvidence key="two" execId="7" nodeId="fan" instanceNo={2} refreshKey={1} onAuthExpired={noop} onClose={noop} />)
    await screen.findByRole('button', { name: '第 6 轮' })
    resolveOld(ok({ ...projection, output: 'stale-instance' }))
    await user.click(screen.getByRole('button', { name: '第 6 轮' }))
    await waitFor(() => expect(screen.getByTestId(TID.attemptEvidence)).toHaveTextContent('round-six'))
    resolveRound(ok({ ...attempt, output: 'stale-round' }))
    await waitFor(() => expect(screen.getByTestId(TID.attemptEvidence)).not.toHaveTextContent('stale-round'))
    expect(screen.getByTestId(TID.nodeEvidence)).not.toHaveTextContent('stale-instance')
  })
})

describe('完整审计与 trace 查询', () => {
  it('审计页面保留真实text原文，大整数payload可核验，解析视图诚实标注', async () => {
    const user = userEvent.setup()
    const raw = JSON.stringify({ items: [audit(1)], next_cursor: '' }).replace('"actual-payload"', '9007199254740993')
    vi.stubGlobal('fetch', vi.fn(async () => new Response(raw)))
    render(<AuditEvidence executionId="7" refreshKey={1} onAuthExpired={noop} onTrace={noop} />)
    const summary = await screen.findByText('审计第 1 页服务端响应原文')
    await user.click(summary)
    expect(summary.parentElement).toHaveTextContent('9007199254740993')
    await user.click(screen.getByText('模型调用已完成'))
    expect(screen.getByText('解析后展示；大整数等精确数值请核对服务端响应原文。')).toBeInTheDocument()
  })

  it('双页各1500ms跨2500ms轮询时第二页仍完成，不被新poll取消或饿死', async () => {
    vi.useFakeTimers()
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => new Promise<Response>((resolve) => {
      setTimeout(() => resolve(ok(String(input).includes('cursor=') ? { items: [audit(101)], next_cursor: '' } : { items: Array.from({ length: 100 }, (_, i) => audit(i + 1)), next_cursor: 'next' })), 1500)
    }))
    vi.stubGlobal('fetch', fetchMock)
    const draw = (refreshKey: number) => <AuditEvidence executionId="7" refreshKey={refreshKey} onAuthExpired={noop} onTrace={noop} />
    const view = render(draw(1))
    await act(() => vi.advanceTimersByTimeAsync(1500))
    fireEvent.click(screen.getByTestId(TID.auditMore))
    await act(() => vi.advanceTimersByTimeAsync(2500))
    view.rerender(draw(2))
    await act(() => vi.advanceTimersByTimeAsync(500))
    expect(screen.getByTestId(TID.auditCount)).toHaveTextContent('已加载 101 条 · 已到末页')
    expect(fetchMock).toHaveBeenCalledTimes(3)
  })

  it('超过100条按游标可达，刷新保留深度，事件原码/payload/链/用量不丢弃', async () => {
    const user = userEvent.setup()
    const calls: string[] = []
    const onTrace = vi.fn()
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input); calls.push(url)
      return url.includes('cursor=') ? ok({ items: [audit(100), audit(101, { event_type: 'unknown.event' })], next_cursor: '' }) : ok({ items: Array.from({ length: 100 }, (_, i) => audit(i + 1)), next_cursor: 'cursor+next' })
    }))
    render(<AuditEvidence executionId="7" refreshKey={1} onAuthExpired={noop} onTrace={onTrace} />)
    await waitFor(() => expect(screen.getByTestId(TID.auditCount)).toHaveTextContent('已加载 100 条 · 仍有更多'))
    await user.click(screen.getByTestId(TID.auditMore))
    await waitFor(() => expect(screen.getByTestId(TID.auditCount)).toHaveTextContent('已加载 101 条 · 已到末页'))
    expect(calls).toContain('/api/v1/audit?execution_id=7&limit=100&cursor=cursor%2Bnext')
    expect(screen.getByTestId(tid.auditRow('unknown.event'))).toHaveTextContent('未收录解释的事件')
    const row = screen.getAllByTestId(tid.auditRow('llm.call.completed'))[0]
    await user.click(within(row).getByText('模型调用已完成'))
    expect(row).toHaveTextContent('actual-payload')
    expect(row).toHaveTextContent('b'.repeat(64))
    expect(row).toHaveTextContent('成本 1.25 CNY · 估算')
    await user.click(within(row).getByRole('button', { name: /查询 trace/ }))
    expect(onTrace).toHaveBeenCalledWith('a'.repeat(32))
    await user.click(screen.getByRole('button', { name: '刷新审计' }))
    await waitFor(() => expect(calls.filter((url) => url.includes('cursor=')).length).toBe(2))
    expect(screen.getByTestId(TID.auditCount)).toHaveTextContent('已加载 101 条')
  })

  it('trace只使用真实trace维度，零值USD与NULL未知分开；失败保留结果可重试', async () => {
    const user = userEvent.setup()
    let fail = false
    const calls: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      calls.push(String(input))
      if (fail) return ok({ error: 'backend_failed' }, 500)
      return ok({ items: [audit(1, { cost_amount: 0, cost_currency: 'USD', cost_estimated: false }), audit(2, { event_type: 'tool.call.completed', model: null, prompt_tokens: null, completion_tokens: null, total_tokens: null, cost_amount: null, cost_currency: null, cost_estimated: false })], next_cursor: '' })
    }))
    render(<AuditEvidence traceId={'a'.repeat(32)} refreshKey={1} onAuthExpired={noop} onTrace={noop} />)
    await screen.findByText('成本 0 USD')
    expect(screen.getByTestId(tid.auditRow('tool.call.completed'))).toHaveTextContent('成本 未记录')
    expect(calls[0]).toBe(`/api/v1/audit?trace_id=${'a'.repeat(32)}&limit=100`)
    expect(calls[0]).not.toContain('execution_id')
    fail = true
    await user.click(screen.getByRole('button', { name: '刷新审计' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('保留上次读取结果')
    expect(screen.getByText('成本 0 USD')).toBeInTheDocument()
  })
})
