// API 客户端:同源 /api/v1,Bearer + sessionStorage(载体与审批台同
// 拍板,D-t44 #1;sessionStorage 每标签页独立,画布自带登录条)。
// 401 统一抛 ApiError(status=401),App 层清 token 回登录。
import type { WorkflowDoc } from './model/dsl.ts'

const TOKEN_KEY = 'braid_canvas_token'

export interface Issue {
  code: string
  node?: string
  ref?: string
  detail: string
}

export class ApiError extends Error {
  status: number
  code: string
  issues: Issue[]
  /** 回滚 409 族(not_rollbackable/blocked_inflight/too_large)携带的补充说明。 */
  detail?: string
  /** blocked_inflight 的行清单("node_id:instance_no")。 */
  blocked?: string[]

  constructor(status: number, code: string, issues: Issue[], detail?: string, blocked?: string[]) {
    super(`api ${status} ${code}`)
    this.status = status
    this.code = code
    this.issues = issues
    this.detail = detail
    this.blocked = blocked
  }
}

export const tokenStore = {
  get(): string | null {
    try {
      return sessionStorage.getItem(TOKEN_KEY)
    } catch {
      return null
    }
  },
  set(token: string) {
    try {
      sessionStorage.setItem(TOKEN_KEY, token)
    } catch {
      /* 私密模式等存储不可用:仅本次内存态生效 */
    }
  },
  clear() {
    try {
      sessionStorage.removeItem(TOKEN_KEY)
    } catch {
      /* 同上 */
    }
  },
}

async function call<T>(method: string, path: string, body?: string, extraHeaders?: Record<string, string>): Promise<T> {
  const headers: Record<string, string> = { ...extraHeaders }
  const token = tokenStore.get()
  if (token !== null) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, { method, headers, body })
  if (!res.ok) {
    let code = `http_${res.status}`
    let issues: Issue[] = []
    try {
      const payload = (await res.json()) as {
        error?: string
        issues?: Issue[]
        detail?: string
        blocked?: string[]
      }
      if (payload.error !== undefined) code = payload.error
      if (Array.isArray(payload.issues)) issues = payload.issues
      throw new ApiError(res.status, code, issues, payload.detail, payload.blocked)
    } catch (err) {
      if (err instanceof ApiError) throw err
      /* 非 JSON 错误体:保底 code */
    }
    throw new ApiError(res.status, code, issues)
  }
  if (res.status === 204) return undefined as T
  const text = await res.text()
  return (text === '' ? undefined : JSON.parse(text)) as T
}

// ---- 契约面(internal/api definitionDTO / login 响应) ----

export interface DraftSummary {
  id: string
  name: string
  version: number | null
  status: string
  description: string
  updated_at: string
}

export interface DraftDetail extends DraftSummary {
  dsl: WorkflowDoc
}

export interface LoginResponse {
  token: string
  token_type: string
  expires_at: string
}

// ---- 契约面(T16.6 执行域:executionDTO / executionDetailDTO /
// rollbackDTO / approvalDetailDTO / auditEventDTO,字段镜像 internal/api)----

/** 状态词表(DDL ENUM,执行/节点共用子集;渲染层只显不判)。 */
export const EXECUTION_STATUSES = ['PENDING', 'RUNNING', 'WAITING_APPROVAL', 'SUCCEEDED', 'FAILED', 'CANCELLED'] as const

export interface Page {
  limit: number
  offset: number
  total: number
}

export interface ExecutionSummary {
  id: string
  definition_id: string
  definition_name: string
  definition_version: number
  status: string
  trigger_type: string
  triggered_by: string
  error_code: string | null
  error_message: string | null
  created_at: string
  started_at: string | null
  finished_at: string | null
}

export interface NodeExecution {
  id: string
  node_id: string
  instance_no: number
  type: string
  status: string
  attempts: number
  max_attempts: number
  error_code: string | null
  error_message: string | null
  started_at: string | null
  finished_at: string | null
}

export interface ExecutionDetail extends ExecutionSummary {
  definition_hash: string
  input: unknown
  outputs: unknown
  nodes: NodeExecution[]
  audit: { exec_date: string; trace_id: string | null }
}

export interface TriggerAck {
  id: string
  definition_name: string
  definition_version: number
  status: string
  trigger_type: string
  triggered_by: string
}

export interface RollbackAck {
  execution_id: string
  target_node_id: string
  rerun_target: boolean
  reset_count: number
  /** T19.6:楔停收敛的 EXECUTING/WA 残留行数(0 = 非楔停回滚/零残留)。 */
  quiesced_count: number
}

/** 取消响应(T19.3);cancelled_count = 楔停的节点行数(控制行+实例行)。 */
export interface CancelAck {
  execution_id: string
  from_status: string
  cancelled_count: number
}

export interface ApprovalDecisionRow {
  id: string
  execution_id: string
  node_id: string
  round: number
  decision: string
  decided_by: string
  comment: string | null
  decided_at: string
  /** omitempty:仅终轮投票行携带 true,中间票整字段缺席。 */
  final?: boolean
}

export interface ApprovalDetail {
  execution_id: string
  node_id: string
  node_status: string
  deadline_at: string | null
  approvers?: string[]
  /** omitempty:缺省 = any 语义(approvalmode.go)。 */
  mode?: string
  timeout_hours?: number
  on_timeout?: string
  instructions?: string
  pending_approvers: string[]
  decisions: ApprovalDecisionRow[]
}

/** 待办审批行(GET /approvals/pending;execution_id 字符串化契约同 definitionDTO)。
 *  requested_at = 进入 WAITING_APPROVAL 的时刻(后端 status_changed_at):
 *  回滚重跑后 started_at 停在首跑,等待起点随最新一轮走。 */
export interface PendingApproval {
  execution_id: string
  node_id: string
  definition_name: string
  definition_version: number
  execution_status: string
  deadline_at: string
  requested_at: string
}

export interface AuditEvent {
  id: string
  exec_date: string
  event_at: string
  created_at: string
  event_type: string
  execution_id: string
  node_execution_id: string | null
  node_id: string | null
  attempt_id: string | null
  actor_type: string
  actor_id: string
  trace_id: string | null
  pii_masked: boolean
  prev_hash: string
  row_hash: string | null
  payload: unknown
}

// 端点全字面量(不拼前缀):bundle 端点锚与 grep 语义都锚在完整路径上
// (web_test canvas bundle 锚同 app.js 纪律)。

/** 定义列表翻页单页上限 = 服务端 maxPageLimit(internal/api/handlers.go),
 * 一次请求吃满一页,少发 round-trip。 */
const DEFINITION_PAGE_LIMIT = 100

/** 深翻页防护上界,镜像服务端 maxPageOffset(handlers.go):服务端对
 * offset>10000 回 400 bad_offset,不在此钳制收口,翻页会把列表打进错误态。 */
const DEFINITION_MAX_OFFSET = 10000

/** 定义列表翻页取全(审计 F03:缺省页 20 条,第 21 条草稿/已发布此前在
 * 侧栏与触发下拉静默消失)。契约 = 执行面翻页器同款 page envelope
 * ({items, page:{limit,offset,total}},server_test.go 钉死形状),差异仅在
 * 消费策略:定义列表是"读"不是"扫",下拉与侧栏吃全量,不做翻页 UI。
 * total 缺席/非数 → 退化单页(与执行面 page?.total 降级同口径:宁可少显,
 * 不假造已取全)。offset 越过服务端深翻页上界即截断(服务端 maxPageOffset
 * 的 O(offset) 扫描防护,单工作区定义到该量级属另行立项的规模问题)。 */
async function listAllDefinitions(baseQuery: string): Promise<DraftSummary[]> {
  const items: DraftSummary[] = []
  const seen = new Set<string>()
  let offset = 0
  for (;;) {
    const res = await call<{ items?: DraftSummary[]; page?: Page }>(
      'GET',
      `${baseQuery}&limit=${DEFINITION_PAGE_LIMIT}&offset=${offset}`,
    )
    const batch = Array.isArray(res.items) ? res.items : []
    // 两页之间并发写入会平移 offset 窗口拉到重复行:按 id 收敛(read model
    // 每行一次;同名多版本 id 本就不同,不误伤)。
    for (const it of batch) {
      if (!seen.has(it.id)) {
        seen.add(it.id)
        items.push(it)
      }
    }
    const total = typeof res.page?.total === 'number' ? res.page.total : items.length
    offset += batch.length
    if (batch.length === 0 || offset >= total || offset > DEFINITION_MAX_OFFSET) return items
  }
}

export const api = {
  login(username: string, password: string): Promise<LoginResponse> {
    return call('POST', '/api/v1/auth/login', JSON.stringify({ username, password }))
  },
  listDrafts(): Promise<{ items: DraftSummary[] }> {
    return listAllDefinitions('/api/v1/definitions?status=DRAFT').then((items) => ({ items }))
  },
  getDraft(id: string): Promise<DraftDetail> {
    return call('GET', `/api/v1/drafts/${id}`)
  },
  createDraft(serialized: string): Promise<DraftDetail> {
    return call('POST', '/api/v1/definitions', serialized)
  },
  updateDraft(id: string, serialized: string): Promise<DraftDetail> {
    return call('PUT', `/api/v1/drafts/${id}`, serialized)
  },
  publishDraft(id: string): Promise<DraftDetail> {
    return call('POST', `/api/v1/drafts/${id}/publish`)
  },
  // ---- 执行域(T16.6)----
  listPublished(): Promise<{ items: DraftSummary[] }> {
    return listAllDefinitions('/api/v1/definitions?status=PUBLISHED').then((items) => ({ items }))
  },
  getVersion(name: string, version: number): Promise<DraftDetail> {
    return call('GET', `/api/v1/definitions/${name}/versions/${version}`)
  },
  listExecutions(query?: { status?: string; name?: string; limit?: number; offset?: number }): Promise<{
    items: ExecutionSummary[]
    page: Page
  }> {
    const params = new URLSearchParams()
    if (query?.status !== undefined && query.status !== '') params.set('status', query.status)
    if (query?.name !== undefined && query.name !== '') params.set('name', query.name)
    if (query?.limit !== undefined) params.set('limit', String(query.limit))
    if (query?.offset !== undefined) params.set('offset', String(query.offset))
    const qs = params.toString()
    return call('GET', `/api/v1/executions${qs === '' ? '' : `?${qs}`}`)
  },
  getExecution(id: string): Promise<ExecutionDetail> {
    return call('GET', `/api/v1/executions/${id}`)
  },
  triggerExecution(definitionName: string, input: unknown, idempotencyKey: string): Promise<TriggerAck> {
    // 幂等键由 UI 每次提交新生成(crypto.randomUUID):双击/重放收敛为
    // 200 原执行回显,而非双执行(服务端 uk_exec_idem 永久窗)。
    return call(
      'POST',
      '/api/v1/executions',
      JSON.stringify({ definition_name: definitionName, input }),
      { 'Idempotency-Key': idempotencyKey },
    )
  },
  rollbackExecution(id: string, nodeId: string, quiesce = false): Promise<RollbackAck> {
    // T19.6:quiesce=true → EXECUTING/WA 残留同事务楔停(缺省 false = 服务端
    // 409 blocked_inflight 等后台收敛,等待保值语义)。
    return call(
      'POST',
      `/api/v1/executions/${id}/rollback`,
      JSON.stringify(quiesce ? { node_id: nodeId, quiesce: true } : { node_id: nodeId }),
    )
  },
  resumeExecution(id: string, quiesce = false): Promise<RollbackAck> {
    return quiesce
      ? call('POST', `/api/v1/executions/${id}/resume`, JSON.stringify({ quiesce: true }))
      : call('POST', `/api/v1/executions/${id}/resume`)
  },
  cancelExecution(id: string): Promise<CancelAck> {
    return call('POST', `/api/v1/executions/${id}/cancel`)
  },
  getApproval(executionId: string, nodeId: string): Promise<ApprovalDetail> {
    return call('GET', `/api/v1/executions/${executionId}/approvals/${nodeId}`)
  },
  // ---- 审批待办面(T21.2:旧审批台三件套的端点,零新增)----
  listPendingApprovals(query?: { limit?: number; offset?: number }): Promise<{
    items: PendingApproval[]
    page: Page
  }> {
    const params = new URLSearchParams()
    if (query?.limit !== undefined) params.set('limit', String(query.limit))
    if (query?.offset !== undefined) params.set('offset', String(query.offset))
    const qs = params.toString()
    return call('GET', `/api/v1/approvals/pending${qs === '' ? '' : `?${qs}`}`)
  },
  /** decision 走请求面词表 approve|reject(服务端 decisionFace 反向映射)。 */
  decideApproval(
    executionId: string,
    nodeId: string,
    decision: 'approve' | 'reject',
    comment: string,
  ): Promise<ApprovalDecisionRow> {
    return call(
      'POST',
      `/api/v1/executions/${executionId}/approvals/${nodeId}/decision`,
      JSON.stringify({ decision, comment }),
    )
  },
  listAuditEvents(executionId: string, limit?: number): Promise<{ items: AuditEvent[]; next_cursor: string }> {
    const l = limit === undefined ? '' : `&limit=${limit}`
    return call('GET', `/api/v1/audit?execution_id=${executionId}${l}`)
  },
}
