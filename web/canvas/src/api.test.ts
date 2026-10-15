// api 客户端执行域契约测试(T16.6):路径字面量、请求形状(Bearer/
// Idempotency-Key/体)、响应解包、409 族 detail/blocked 承载。fetch 全
// stub(同 App.test.tsx 习语);断言的形状 = internal/api DTO 的镜像面。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, tokenStore, type DraftSummary } from './api.ts'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

let calls: { input: RequestInfo | URL; init?: RequestInit }[] = []

function stubFetch(responder: (input: RequestInfo | URL, init?: RequestInit) => Response | Promise<Response>) {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input, init })
      return responder(input, init)
    }),
  )
}

beforeEach(() => {
  tokenStore.set('tok-1')
})

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
})

describe('execution domain client', () => {
  it('listExecutions:裸调用不带查询串,带过滤时拼 status/name/limit', async () => {
    stubFetch(() => ok({ items: [], page: { limit: 20, offset: 0, total: 0 } }))
    await api.listExecutions()
    expect(String(calls[0].input)).toBe('/api/v1/executions')

    await api.listExecutions({ status: 'FAILED', name: 'wf', limit: 5 })
    expect(String(calls[1].input)).toBe('/api/v1/executions?status=FAILED&name=wf&limit=5')
  })

  it('getExecution:详情透传 nodes/audit 形状', async () => {
    const detail = {
      id: '7',
      definition_name: 'wf',
      nodes: [{ id: '8', node_id: 'gate', instance_no: 0, status: 'WAITING_APPROVAL' }],
      audit: { exec_date: '2026-10-04', trace_id: null },
    }
    stubFetch(() => ok(detail))
    const out = await api.getExecution('7')
    expect(String(calls[0].input)).toBe('/api/v1/executions/7')
    expect(out.nodes[0].node_id).toBe('gate')
    expect(out.audit.exec_date).toBe('2026-10-04')
  })

  it('triggerExecution:Bearer + Idempotency-Key + 体形状', async () => {
    stubFetch(() => ok({ id: '9', status: 'PENDING' }, 201))
    await api.triggerExecution('wf', { docs: ['a'] }, 'key-1')
    const { input, init } = calls[0]
    expect(String(input)).toBe('/api/v1/executions')
    expect(init?.method).toBe('POST')
    const headers = init?.headers as Record<string, string>
    expect(headers.Authorization).toBe('Bearer tok-1')
    expect(headers['Idempotency-Key']).toBe('key-1')
    expect(JSON.parse(String(init?.body))).toEqual({ definition_name: 'wf', input: { docs: ['a'] } })
  })

  it('rollbackExecution:POST rollback + node_id 体;resume 无体;quiesce 旗标透传', async () => {
    stubFetch(() => ok({ execution_id: '7', target_node_id: 'gate', rerun_target: true, reset_count: 2, quiesced_count: 0 }, 201))
    await api.rollbackExecution('7', 'gate')
    expect(String(calls[0].input)).toBe('/api/v1/executions/7/rollback')
    expect(calls[0].init?.method).toBe('POST')
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ node_id: 'gate' })

    await api.resumeExecution('7')
    expect(String(calls[1].input)).toBe('/api/v1/executions/7/resume')
    expect(calls[1].init?.method).toBe('POST')
    expect(calls[1].init?.body).toBeUndefined()

    // T19.6:quiesce=true 才进体(缺省路径体形状逐字不变)。
    await api.rollbackExecution('7', 'gate', true)
    expect(JSON.parse(String(calls[2].init?.body))).toEqual({ node_id: 'gate', quiesce: true })
    await api.resumeExecution('7', true)
    expect(JSON.parse(String(calls[3].init?.body))).toEqual({ quiesce: true })
  })

  it('getApproval / listAuditEvents:子资源路径', async () => {
    stubFetch(() => ok({ pending_approvers: ['bob'], decisions: [] }))
    const ap = await api.getApproval('7', 'sign')
    expect(String(calls[0].input)).toBe('/api/v1/executions/7/approvals/sign')
    expect(ap.pending_approvers).toEqual(['bob'])

    stubFetch(() => ok({ items: [], next_cursor: '' }))
    await api.listAuditEvents('7')
    expect(String(calls[0].input)).toBe('/api/v1/audit?execution_id=7')
    await api.listAuditEvents('7', 100)
    expect(String(calls[1].input)).toBe('/api/v1/audit?execution_id=7&limit=100')
  })

  it('409 blocked_inflight:detail 与 blocked 清单进 ApiError', async () => {
    stubFetch(
      () =>
        new Response(
          JSON.stringify({ error: 'rollback_blocked_inflight', detail: 'inflight residue', blocked: ['fan:2'] }),
          { status: 409, headers: { 'Content-Type': 'application/json' } },
        ),
    )
    const err = await api.rollbackExecution('7', 'gate').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    const ae = err as ApiError
    expect(ae.status).toBe(409)
    expect(ae.code).toBe('rollback_blocked_inflight')
    expect(ae.detail).toBe('inflight residue')
    expect(ae.blocked).toEqual(['fan:2'])
  })

  it('非 JSON 错误体:保底 code 不抛二级异常', async () => {
    stubFetch(() => new Response('gateway text', { status: 502 }))
    const err = await api.resumeExecution('7').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).code).toBe('http_502')
  })

  it('listPublished / getVersion:定义面路径(翻页参数随行)', async () => {
    stubFetch(() => ok({ items: [], page: { limit: 100, offset: 0, total: 0 } }))
    await api.listPublished()
    expect(String(calls[0].input)).toBe('/api/v1/definitions?status=PUBLISHED&limit=100&offset=0')

    stubFetch(() => ok({ id: '1', dsl: {} }))
    await api.getVersion('wf', 3)
    expect(String(calls[0].input)).toBe('/api/v1/definitions/wf/versions/3')
  })
})

describe('definition list paging(T22.2 审计 F03:超 20 条全量可达)', () => {
  /** mock 契约用真实 page envelope 形状(internal/api handlers gin.H 同款)。 */
  function envelope(items: unknown[], total = items.length): Response {
    return ok({ items, page: { limit: 100, offset: 0, total } })
  }

  function draft(i: number): DraftSummary {
    return { id: String(i), name: `wf-${i}`, version: 1, status: 'DRAFT', description: '', updated_at: '2026-10-04T05:00:00Z' }
  }

  it('21 条场景:limit=100 单请求一次取全(审计 repro:此前只显 20)', async () => {
    const items = Array.from({ length: 21 }, (_, i) => draft(i))
    stubFetch(() => envelope(items))
    const { items: out } = await api.listPublished()
    expect(out).toHaveLength(21)
    expect(String(calls[0].input)).toBe('/api/v1/definitions?status=PUBLISHED&limit=100&offset=0')
    expect(calls).toHaveLength(1)
  })

  it('total 超单页:按 envelope 翻到取全', async () => {
    const page1 = Array.from({ length: 100 }, (_, i) => draft(i))
    const page2 = Array.from({ length: 21 }, (_, i) => draft(100 + i))
    stubFetch((input) => (String(input).endsWith('offset=0') ? envelope(page1, 121) : envelope(page2, 121)))
    const { items: out } = await api.listDrafts()
    expect(out).toHaveLength(121)
    expect(calls.map((c) => String(c.input))).toEqual([
      '/api/v1/definitions?status=DRAFT&limit=100&offset=0',
      '/api/v1/definitions?status=DRAFT&limit=100&offset=100',
    ])
  })

  it('page 缺席:退化单页,不空转', async () => {
    stubFetch(() => ok({ items: [draft(1)] }))
    const { items: out } = await api.listDrafts()
    expect(out).toHaveLength(1)
    expect(calls).toHaveLength(1)
  })

  it('服务端空页:立即收口(偏移越过 total 时不死循环)', async () => {
    stubFetch(() => envelope([], 0))
    const { items: out } = await api.listDrafts()
    expect(out).toHaveLength(0)
    expect(calls).toHaveLength(1)
  })
})
