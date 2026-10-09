// 执行详情(T21.1 Lane B 重做)—— 纵向分层,眼睛从上到下一次读完:
//   ① 身份:状态芯片(色相 + 形状)+ 裸 ID/定义版本(与日志/审计同语言)
//   ② 执行级错误:就近,不埋进表格
//   ③ 处置条:回滚目标(在时间带上选点)/ 静默在飞 / 回滚 · 续跑 · 取消(行内确认)
//   ④ 执行时间带(signature move):节点状态与"等了多久"的唯一真源
//   ④′ 定义版本图(可折叠,展开才拉取):"当时声明的形状"——DAG 拓扑不可替代
//   ⑤ 会签中间态:待投名单 + 决定时间线(名单外 403 静默收起)
//   ⑥ 终态输出 → 审计链(append-only,逐行可数)
//
// 轮询:非终态 2.5s;FAILED 保持 10s 慢速(可被本页回滚/续跑、他人操作、
// reaper 兜底翻活,恒停会冻结在过期帧);SUCCEEDED/CANCELLED 停表。
// 零后端新端点:全部字段来自既有 executionDetailDTO / approvalDetailDTO / auditEventDTO。
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Ban, Play, RefreshCw, ScrollText, TriangleAlert, Undo2 } from 'lucide-react'
import {
  ApiError,
  api,
  type ApprovalDetail,
  type AuditEvent,
  type ExecutionDetail as ExecutionDetailData,
  type NodeExecution,
} from './api.ts'
import { DefinitionGraph } from './DefinitionGraph.tsx'
import { ExecutionRibbon } from './ExecutionRibbon.tsx'
import { Checkbox } from './ui/field.tsx'
import { Alert, Badge, Button, KV, Panel, PanelHead, StatusChip } from './ui/primitives.tsx'
import { DECISION_LABEL, ON_TIMEOUT_LABEL, TRIGGER_LABEL } from './ui/status.ts'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'

const POLL_MS = 2500
const FAILED_POLL_MS = 10_000

/** 可处置(回滚/续跑)的终态;取消面 = 仍在推进的活执行。 */
const TERMINAL_ACTIONABLE = ['FAILED', 'SUCCEEDED', 'CANCELLED']
const CANCELLABLE = ['RUNNING', 'WAITING_APPROVAL']
/** 执行仍可能推进 → 时间带右边界跟着 now 走。 */
const LIVE = ['PENDING', 'RUNNING', 'WAITING_APPROVAL']

/** 节点级聚合的优先级(失败优先):并行多实例行 → 单节点一色,给定义版本图注入。 */
const STATUS_RANK = ['FAILED', 'EXECUTING', 'WAITING_APPROVAL', 'PENDING', 'SUCCEEDED', 'SKIPPED', 'CANCELLED']

function rank(status: string): number {
  const i = STATUS_RANK.indexOf(status)
  return i === -1 ? STATUS_RANK.length : i
}

/** 节点 id → 当前执行的聚合状态(缺省 = 该节点这次没有执行行)。 */
function nodeStatusIndex(rows: NodeExecution[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of rows) {
    const cur = out[r.node_id]
    if (cur === undefined || rank(r.status) < rank(cur)) out[r.node_id] = r.status
  }
  return out
}

export function ExecutionDetail({ execId, onAuthExpired }: { execId: string; onAuthExpired: () => void }) {
  const [detail, setDetail] = useState<ExecutionDetailData | null>(null)
  const [approvals, setApprovals] = useState<Record<string, ApprovalDetail | null>>({})
  const [audit, setAudit] = useState<AuditEvent[]>([])
  const [target, setTarget] = useState<string | null>(null)
  // T19.6 quiesce 旗标:回滚/续跑时把 EXECUTING/等待审批的残留节点同事务
  // 楔停(缺省不勾 = 服务端 409 等后台收敛,保值等待)。
  const [quiesce, setQuiesce] = useState(false)
  const [flash, setFlash] = useState('')
  const [error, setError] = useState<{ text: string; blocked?: string[] } | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirmCancel, setConfirmCancel] = useState(false)
  const confirmRef = useRef<HTMLDivElement>(null)

  const load = useCallback(async () => {
    let d: ExecutionDetailData | null = null
    try {
      d = await api.getExecution(execId)
      setDetail(d)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthExpired()
      return // 瞬态读失败:保留上一帧,下个轮询窗自愈
    }
    // 审批子资源:仅 WAITING_APPROVAL 的 approval 行;名单外 403 = 静默收起。
    // 已离开挂起态的行同步剪枝(否则节点完成后残留「待投」幽灵帧)。
    const waiting = (d.nodes ?? []).filter((n) => n.type === 'approval' && n.status === 'WAITING_APPROVAL')
    const waitingIds = new Set(waiting.map((n) => n.node_id))
    setApprovals((prev) => {
      const next: Record<string, ApprovalDetail | null> = {}
      let changed = false
      for (const k of Object.keys(prev)) {
        if (waitingIds.has(k)) next[k] = prev[k]
        else changed = true
      }
      return changed ? next : prev
    })
    for (const n of waiting) {
      try {
        const ap = await api.getApproval(execId, n.node_id)
        setApprovals((prev) => ({ ...prev, [n.node_id]: ap }))
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          onAuthExpired()
          return
        }
        setApprovals((prev) => ({ ...prev, [n.node_id]: null }))
      }
    }
    try {
      const a = await api.listAuditEvents(execId, 100)
      setAudit(Array.isArray(a.items) ? a.items : [])
    } catch {
      /* 审计区读失败不阻断详情面 */
    }
  }, [execId, onAuthExpired])

  useEffect(() => {
    void load()
  }, [load])

  const stopPolling = detail !== null && ['SUCCEEDED', 'CANCELLED'].includes(detail.status)
  const pollMs = detail !== null && detail.status === 'FAILED' ? FAILED_POLL_MS : POLL_MS
  useEffect(() => {
    if (stopPolling) return
    const t = setInterval(() => void load(), pollMs)
    return () => clearInterval(t)
  }, [load, stopPolling, pollMs])

  // 确认条是一次性的浮起层:焦点移进去,Esc 收起(浮层契约)。
  useEffect(() => {
    if (confirmCancel) confirmRef.current?.focus()
  }, [confirmCancel])

  const faceError = useCallback((err: unknown) => {
    if (err instanceof ApiError) {
      setError({ text: [err.code, err.detail].filter((x) => x !== undefined && x !== '').join(':'), blocked: err.blocked })
    } else {
      setError({ text: 'network_error' })
    }
  }, [])

  const runCancel = useCallback(async () => {
    setBusy(true)
    setError(null)
    setFlash('')
    try {
      const ack = await api.cancelExecution(execId)
      setFlash(`已取消(楔停 ${ack.cancelled_count} 行)`)
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthExpired()
        return
      }
      faceError(err)
    } finally {
      setBusy(false)
      setConfirmCancel(false)
    }
  }, [execId, load, onAuthExpired, faceError])

  const runAction = useCallback(
    async (fn: () => Promise<{ target_node_id: string; reset_count: number; rerun_target: boolean; quiesced_count: number }>) => {
      setBusy(true)
      setError(null)
      setFlash('')
      try {
        const ack = await fn()
        setFlash(
          `已回滚到 ${ack.target_node_id}(重置 ${ack.reset_count} 行${ack.rerun_target ? ',目标节点重执行' : ''}${
            ack.quiesced_count > 0 ? `,静默在飞 ${ack.quiesced_count} 行` : ''
          })`,
        )
        await load()
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          onAuthExpired()
          return
        }
        faceError(err)
      } finally {
        setBusy(false)
      }
    },
    [load, onAuthExpired, faceError],
  )

  if (detail === null) {
    return (
      <main className="exec-detail" data-testid={TID.execDetail}>
        <p className="text-sm text-ink-2">
          加载执行 <code className="mono">#{execId}</code>…
        </p>
      </main>
    )
  }

  const canAct = TERMINAL_ACTIONABLE.includes(detail.status) && !busy
  const canCancel = CANCELLABLE.includes(detail.status) && !busy
  const statusRows = detail.nodes ?? []
  const approvalRows = statusRows.filter((n) => n.type === 'approval' && n.status === 'WAITING_APPROVAL' && approvals[n.node_id] != null)

  return (
    <main className="exec-detail" data-testid={TID.execDetail}>
      {/* ① 身份 */}
      <header className="exec-head">
        <div className="exec-head-title">
          <StatusChip status={detail.status} testId={TID.execStatus} />
          <code className="panel-id">
            #{detail.id} · {detail.definition_name} v{detail.definition_version}
          </code>
          <span className="ml-auto flex items-center gap-2">
            <Button variant="quiet" size="sm" icon={RefreshCw} disabled={busy} onClick={() => void load()}>
              刷新
            </Button>
          </span>
        </div>
        <dl className="exec-head-meta">
          <Meta k="触发">
            {TRIGGER_LABEL[detail.trigger_type] ?? detail.trigger_type}
            <span className="mono text-ink-2"> · {detail.triggered_by}</span>
          </Meta>
          <Meta k="创建">
            <Timestamp iso={detail.created_at} />
          </Meta>
          <Meta k="运行">
            <Duration from={detail.started_at} to={detail.finished_at} />
          </Meta>
          {detail.audit.trace_id !== null ? (
            <Meta k="trace_id">
              <code className="mono">{detail.audit.trace_id}</code>
            </Meta>
          ) : null}
        </dl>
      </header>

      {/* ② 执行级错误:就近呈现 */}
      {detail.error_code !== null ? (
        <Alert tone="error" icon={TriangleAlert}>
          <strong>执行错误</strong>
          <code className="mono ml-2">{detail.error_code}</code>
          {detail.error_message !== null && detail.error_message !== '' ? (
            <span className="ml-2 text-ink-1">{detail.error_message}</span>
          ) : null}
        </Alert>
      ) : null}

      {/* ③ 处置条 */}
      <section className="panel exec-actions">
        <div className="panel-body exec-actions-body">
          <div className="exec-target">
            <span className="micro">回滚目标</span>
            {target !== null ? (
              <code className="mono text-ink-0">{target}</code>
            ) : (
              <span className="text-xs text-ink-2">点击时间带中的节点行选择</span>
            )}
          </div>
          <div className="exec-actions-right">
            <Checkbox
              label="静默在飞"
              hint="勾选后回滚/续跑把在飞残留同事务楔停;不勾等后台收敛(409)"
              checked={quiesce}
              onChange={(e) => setQuiesce(e.target.checked)}
            />
            <Button
              icon={Undo2}
              data-testid={TID.rollbackSubmit}
              disabled={!canAct || target === null}
              onClick={() => void runAction(() => api.rollbackExecution(execId, target as string, quiesce))}
            >
              回滚{target !== null ? `到 ${target}` : '(先选节点)'}
            </Button>
            <Button
              icon={Play}
              data-testid={TID.resumeSubmit}
              disabled={!canAct}
              title="自动定位失败/取消点继续跑"
              onClick={() => void runAction(() => api.resumeExecution(execId, quiesce))}
            >
              续跑
            </Button>
            {confirmCancel ? (
              <div
                className="exec-confirm"
                ref={confirmRef}
                tabIndex={-1}
                role="group"
                aria-label="确认取消执行"
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    e.stopPropagation()
                    setConfirmCancel(false)
                  }
                }}
              >
                <span className="text-xs text-ink-1">取消会楔停全部在飞节点,不可撤销。</span>
                <Button variant="danger" size="sm" data-testid={TID.cancelSubmit} disabled={busy} onClick={() => void runCancel()}>
                  确认取消
                </Button>
                <Button variant="quiet" size="sm" disabled={busy} onClick={() => setConfirmCancel(false)}>
                  放弃
                </Button>
              </div>
            ) : (
              <Button variant="danger" icon={Ban} disabled={!canCancel} onClick={() => setConfirmCancel(true)}>
                取消执行
              </Button>
            )}
          </div>
        </div>
      </section>

      {flash !== '' ? (
        <p className="exec-flash" role="status" data-testid={TID.execFlash}>
          {flash}
        </p>
      ) : null}
      {error !== null ? (
        <Alert tone="error" icon={TriangleAlert}>
          <strong>操作未完成</strong>
          <code className="mono ml-2">{error.text}</code>
          {error.blocked !== undefined && error.blocked.length > 0 ? (
            <ul className="exec-blocked">
              {error.blocked.map((b) => (
                <li key={b}>
                  <code>{b}</code>
                </li>
              ))}
            </ul>
          ) : null}
        </Alert>
      ) : null}

      {/* ④ 时间带 */}
      <ExecutionRibbon
        nodes={statusRows}
        startedAt={detail.started_at}
        finishedAt={detail.finished_at}
        live={LIVE.includes(detail.status)}
        selectedNodeId={target}
        onSelectNode={setTarget}
      />

      {/* ④′ 当时声明的形状(可折叠:默认收起 = 零请求) */}
      <DefinitionGraph
        name={detail.definition_name}
        version={detail.definition_version}
        statuses={nodeStatusIndex(statusRows)}
        onAuthExpired={onAuthExpired}
      />

      {/* ⑤ 会签中间态 */}
      {approvalRows.map((n) => {
        const ap = approvals[n.node_id]
        return ap === undefined || ap === null ? null : <ApprovalPanel key={n.node_id} ap={ap} />
      })}

      {/* ⑥ 输出 */}
      {detail.outputs !== null && detail.outputs !== undefined ? (
        <Panel>
          <PanelHead title="输出" sub="终态快照" />
          <pre className="exec-json" data-testid={TID.execOutputs}>
            {JSON.stringify(detail.outputs, null, 2)}
          </pre>
        </Panel>
      ) : null}

      {/* ⑥ 审计链 */}
      <Panel>
        <PanelHead
          title="审计事件"
          sub={`${audit.length} 条 · 分区 ${detail.audit.exec_date}`}
          actions={<Badge icon={ScrollText}>append-only</Badge>}
        />
        <div className="exec-table-wrap">
          <table className="table" data-testid={TID.auditList}>
            <thead>
              <tr>
                <th>事件</th>
                <th>节点</th>
                <th>主体</th>
                <th>链</th>
                <th>时间</th>
              </tr>
            </thead>
            <tbody>
              {audit.length === 0 ? (
                <tr>
                  <td colSpan={5} className="text-ink-2">
                    暂无审计事件
                  </td>
                </tr>
              ) : (
                audit.map((ev) => (
                  <tr key={ev.id} data-testid={tid.auditRow(ev.event_type)}>
                    <td>
                      <code>{ev.event_type}</code>
                    </td>
                    <td className="num">{ev.node_id ?? '—'}</td>
                    <td className="num">
                      {ev.actor_type}:{ev.actor_id}
                    </td>
                    <td>
                      <code
                        className="mono"
                        title={ev.row_hash !== null ? `row ${ev.row_hash}\nprev ${ev.prev_hash}` : undefined}
                      >
                        {shortHash(ev.row_hash)}
                      </code>
                    </td>
                    <td>
                      <Timestamp iso={ev.event_at} />
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Panel>
    </main>
  )
}

/** 头部元信息:micro 标签 + 值(等宽数字)。dl > div > dt/dd 是合法分组。 */
function Meta({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div>
      <dt className="micro">{k}</dt>
      <dd>{children}</dd>
    </div>
  )
}

function shortHash(h: string | null): string {
  return h === null || h === '' ? '—' : h.slice(0, 8)
}

/** 会签中间态:待投名单 + 决定时间线(数据面 = approvalDetailDTO)。 */
function ApprovalPanel({ ap }: { ap: ApprovalDetail }) {
  const timeout = ap.on_timeout !== undefined ? (ON_TIMEOUT_LABEL[ap.on_timeout] ?? ap.on_timeout) : null
  return (
    <section className="panel" data-testid={tid.approvalEntry(ap.node_id)}>
      <PanelHead
        title={`审批 · ${ap.mode === 'all' ? '会签' : '或签'}`}
        sub={ap.node_id}
        actions={<StatusChip status={ap.node_status} kind="node" />}
      />
      <div className="panel-body">
        <dl className="exec-kv">
          <KV k="审批人">
            {ap.approvers !== undefined && ap.approvers.length > 0 ? ap.approvers.join('、') : '—'}
          </KV>
          <KV k="待投">
            <span data-testid={TID.pendingApprovers}>{ap.pending_approvers.join(', ')}</span>
          </KV>
          <KV k="时限">
            {ap.deadline_at !== null ? <Timestamp iso={ap.deadline_at} relative={false} /> : '—'}
            {timeout !== null && ap.timeout_hours !== undefined ? (
              <span className="ml-2 text-xs text-ink-2">
                超时 {timeout} · {ap.timeout_hours}h
              </span>
            ) : null}
          </KV>
        </dl>
        {ap.instructions !== undefined && ap.instructions !== '' ? (
          <p className="exec-instructions measure">{ap.instructions}</p>
        ) : null}
        {ap.decisions.length > 0 ? (
          <ol className="exec-decisions">
            {ap.decisions.map((d) => (
              <li key={d.id}>
                <span className="num text-ink-2">第 {d.round} 轮</span>
                <span className="mono text-ink-0">{d.decided_by}</span>
                <span>{DECISION_LABEL[d.decision] ?? d.decision}</span>
                <Timestamp iso={d.decided_at} />
                {d.comment !== null && d.comment !== '' ? <span className="text-ink-1">{d.comment}</span> : null}
                {d.final === true ? <Badge>终票</Badge> : null}
              </li>
            ))}
          </ol>
        ) : (
          <p className="text-xs text-ink-2">暂无投票。</p>
        )}
      </div>
    </section>
  )
}
