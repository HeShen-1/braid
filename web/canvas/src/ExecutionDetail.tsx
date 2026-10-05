// 执行详情(T16.6):节点状态表(并行实例行)+ 审批中间态(会签待投
// 名单,仅名单内审批人可见——403 静默收起)+ 回滚/续跑操作面 + 审计
// 事件区 + 钉版 DSL 只读图(节点状态着色,点击选回滚目标)。
// 轮询:非终态每 2.5s 刷新(执行推进是 outbox→relay→worker 异步链,UI
// 不订阅只轮询——与审批台同形态);终态停表。
import { useCallback, useEffect, useState } from 'react'
import { ReactFlow } from '@xyflow/react'
import {
  ApiError,
  api,
  type ApprovalDetail,
  type AuditEvent,
  type ExecutionDetail as ExecutionDetailData,
} from './api.ts'
import { toCanvas } from './model/convert.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { nodeTypes } from './nodes/NodeViews.tsx'

const POLL_MS = 2500

/** 节点级状态聚合(并行多实例行 → 单节点一色;失败优先)。 */
function nodeStatusOf(rows: { node_id: string; status: string }[], nodeId: string): string | null {
  const ss = rows.filter((r) => r.node_id === nodeId).map((r) => r.status)
  if (ss.length === 0) return null
  for (const s of ['FAILED', 'EXECUTING', 'WAITING_APPROVAL', 'PENDING', 'SUCCEEDED', 'SKIPPED', 'CANCELLED']) {
    if (ss.includes(s)) return s
  }
  return ss[0]
}

const STATUS_COLOR: Record<string, { border: string; bg: string }> = {
  FAILED: { border: '#b91c1c', bg: '#fef2f2' },
  EXECUTING: { border: '#1d4ed8', bg: '#eff6ff' },
  WAITING_APPROVAL: { border: '#b45309', bg: '#fffbeb' },
  PENDING: { border: '#6b7280', bg: '#f9fafb' },
  SUCCEEDED: { border: '#15803d', bg: '#f0fdf4' },
  SKIPPED: { border: '#9ca3af', bg: '#f3f4f6' },
  CANCELLED: { border: '#9ca3af', bg: '#f3f4f6' },
}

export function ExecutionDetail({ execId, onAuthExpired }: { execId: string; onAuthExpired: () => void }) {
  const [detail, setDetail] = useState<ExecutionDetailData | null>(null)
  const [doc, setDoc] = useState<WorkflowDoc | null>(null)
  const [approvals, setApprovals] = useState<Record<string, ApprovalDetail | null>>({})
  const [audit, setAudit] = useState<AuditEvent[]>([])
  const [target, setTarget] = useState<string | null>(null)
  const [flash, setFlash] = useState('')
  const [error, setError] = useState<{ text: string; blocked?: string[] } | null>(null)
  const [busy, setBusy] = useState(false)

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
    // 已离开挂起态的行同步剪枝(对抗审计 MEDIUM:只写不删会让面板在节点
    // 完成后残留「待投」幽灵帧)。
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

  // 轮询停表仅 SUCCEEDED/CANCELLED;FAILED 保持慢速轮询——失败态可被
  // 外部翻活(本页回滚/续跑、他人操作、reaper 兜底),恒停会冻结在过期
  // 帧上;慢速档也自愈「操作成功后单次 load 瞬态失败」的冻结窗(AI review LOW)。
  const stopPolling = detail !== null && ['SUCCEEDED', 'CANCELLED'].includes(detail.status)
  const pollMs = detail !== null && detail.status === 'FAILED' ? 10_000 : POLL_MS
  useEffect(() => {
    if (stopPolling) return
    const t = setInterval(() => void load(), pollMs)
    return () => clearInterval(t)
  }, [load, stopPolling, pollMs])

  // 钉版 DSL 只读图:按 detail 的 name+version 取定义(一次性,失败隐藏)。
  const defName = detail?.definition_name
  const defVersion = detail?.definition_version
  useEffect(() => {
    if (defName === undefined || defVersion === undefined) return
    let cancelled = false
    api
      .getVersion(defName, defVersion)
      .then((d) => {
        if (!cancelled && d.dsl !== undefined && d.dsl !== null) setDoc(d.dsl)
      })
      .catch(() => {
        /* 图缺失不阻断表格式详情 */
      })
    return () => {
      cancelled = true
    }
  }, [defName, defVersion])

  const faceError = useCallback((err: unknown) => {
    if (err instanceof ApiError) {
      setError({ text: [err.code, err.detail].filter((x) => x !== undefined && x !== '').join(':'), blocked: err.blocked })
    } else {
      setError({ text: 'network_error' })
    }
  }, [])

  // T19.3 取消动作:与 runAction 同形,flash 回显楔停行数。
  const runCancel = useCallback(async () => {
    setBusy(true)
    setError(null)
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
    }
  }, [execId, load, onAuthExpired, faceError])

  const runAction = useCallback(
    async (fn: () => Promise<{ target_node_id: string; reset_count: number; rerun_target: boolean }>) => {
      setBusy(true)
      setError(null)
      try {
        const ack = await fn()
        setFlash(`已回滚到 ${ack.target_node_id}(重置 ${ack.reset_count} 行${ack.rerun_target ? ',目标节点重执行' : ''})`)
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
      <main className="canvas-main" data-testid="exec-detail">
        <p>加载执行 #{execId}…</p>
      </main>
    )
  }

  // T19.3:回滚面放开到三终态(FAILED/SUCCEEDED/CANCELLED);取消面 = 非终态
  // 活执行(RUNNING/WA)。SUCCEEDED 续跑无自动定位点(服务端 no_resume_target
  // 分面报错),显式选节点走回滚。
  const canAct = ['FAILED', 'SUCCEEDED', 'CANCELLED'].includes(detail.status) && !busy
  const canCancel = ['RUNNING', 'WAITING_APPROVAL'].includes(detail.status) && !busy
  const statusRows = detail.nodes ?? []
  const graph = doc !== null ? toCanvas(doc) : null

  // 刻意不用 .canvas-main(其 flex 居中 + 本容器 overflow 滚动 = 经典
  // 「居中截断」:滚动起点越过内容顶,顶部控件永远够不着)。
  return (
    <main className="exec-detail" data-testid="exec-detail">
      <div className="panel-head">
        <span className={`bn-badge exec-status st-${detail.status}`} data-testid="exec-status">
          {detail.status}
        </span>
        <code className="panel-id">
          #{detail.id} · {detail.definition_name} v{detail.definition_version}
        </code>
      </div>
      <div className="exec-meta mono">
        <span>触发 {detail.trigger_type} / {detail.triggered_by}</span>
        <span>创建 {detail.created_at}</span>
        {detail.error_code !== null ? <span className="error">{detail.error_code}</span> : null}
      </div>

      <div className="rollback-bar">
        <button
          type="button"
          className="f-add"
          data-testid="rollback-submit"
          disabled={!canAct || target === null}
          onClick={() => void runAction(() => api.rollbackExecution(execId, target as string))}
        >
          回滚{target !== null ? `到 ${target}` : '(先选节点)'}
        </button>
        <button
          type="button"
          className="f-add"
          data-testid="resume-submit"
          disabled={!canAct}
          onClick={() => void runAction(() => api.resumeExecution(execId))}
        >
          续跑(自动定位失败/取消点)
        </button>
        <button
          type="button"
          className="f-add"
          data-testid="cancel-submit"
          disabled={!canCancel}
          onClick={() => void runCancel()}
        >
          取消执行
        </button>
        <button type="button" className="f-add" data-testid="detail-refresh" onClick={() => void load()}>
          刷新
        </button>
      </div>
      {flash !== '' ? (
        <p className="header-flash" role="status" data-testid="exec-flash">
          {flash}
        </p>
      ) : null}
      {error !== null ? (
        <div className="issues" role="alert" data-testid="exec-error">
          <strong>操作未完成</strong>
          <p>{error.text}</p>
          {error.blocked !== undefined && error.blocked.length > 0 ? (
            <ul>
              {error.blocked.map((b) => (
                <li key={b}>
                  <code>{b}</code>
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}

      {graph !== null ? (
        <div className="exec-graph" data-testid="exec-graph">
          <ReactFlow
            nodes={graph.nodes.map((n) => {
              const st = nodeStatusOf(statusRows, n.id)
              const c = st !== null ? STATUS_COLOR[st] : undefined
              return {
                ...n,
                selected: n.id === target,
                draggable: false,
                style: c === undefined ? undefined : { borderColor: c.border, backgroundColor: c.bg },
              }
            })}
            edges={graph.edges.map((e) => ({ ...e, selectable: false }))}
            nodeTypes={nodeTypes}
            fitView
            nodesDraggable={false}
            nodesConnectable={false}
            onNodeClick={(_, n) => setTarget(n.id)}
          >
          </ReactFlow>
        </div>
      ) : null}

      <table className="exec-table" data-testid="node-table">
        <thead>
          <tr>
            <th>节点</th>
            <th>实例</th>
            <th>类型</th>
            <th>状态</th>
            <th>尝试</th>
            <th>错误</th>
            <th>结束于</th>
          </tr>
        </thead>
        <tbody>
          {statusRows.map((n) => (
            <NodeRow key={n.id} n={n} selected={target === n.node_id} onSelect={() => setTarget(n.node_id)} />
          ))}
        </tbody>
      </table>

      {statusRows
        .filter((n) => n.type === 'approval' && n.status === 'WAITING_APPROVAL' && approvals[n.node_id] != null)
        .map((n) => {
          const ap = approvals[n.node_id]
          if (ap === null || ap === undefined) return null
          return <ApprovalState key={n.node_id} ap={ap} />
        })}

      {detail.outputs !== null && detail.outputs !== undefined ? (
        <section className="panel">
          <div className="panel-head">
            <span className="bn-badge">输出</span>
          </div>
          <pre className="editor-json" data-testid="exec-outputs">
            {JSON.stringify(detail.outputs, null, 2)}
          </pre>
        </section>
      ) : null}

      <section className="panel">
        <div className="panel-head">
          <span className="bn-badge">审计事件(近 {audit.length} 条)</span>
          <code className="panel-id">{detail.audit.exec_date}</code>
        </div>
        <table className="exec-table audit-table" data-testid="audit-table">
          <thead>
            <tr>
              <th>事件</th>
              <th>节点</th>
              <th>主体</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            {audit.map((ev, i) => (
              <tr key={ev.id} data-testid={`audit-row-${ev.event_type}`}>
                <td>
                  <code>{ev.event_type}</code>
                </td>
                <td className="mono">{ev.node_id ?? '—'}</td>
                <td className="mono">
                  {ev.actor_type}:{ev.actor_id}
                </td>
                <td className="mono">{i === -1 ? '' : ev.event_at}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
    </main>
  )
}

function NodeRow({ n, selected, onSelect }: { n: ExecutionDetailData['nodes'][number]; selected: boolean; onSelect: () => void }) {
  return (
    <tr
      className={`node-row${selected ? ' selected' : ''}`}
      data-testid={`node-row-${n.node_id}-${n.instance_no}`}
      onClick={onSelect}
    >
      <td className="mono">{n.node_id}</td>
      <td className="mono">{n.instance_no}</td>
      <td>{n.type}</td>
      <td>
        <span className={`exec-status st-${n.status}`}>{n.status}</span>
      </td>
      <td className="mono">
        {n.attempts}/{n.max_attempts}
      </td>
      <td>{n.error_code ?? (n.error_message !== null ? n.error_message : '—')}</td>
      <td className="mono">{n.finished_at ?? '—'}</td>
    </tr>
  )
}

/** 审批中间态:会签待投名单 + 决定时间线(数据面 = approvalDetailDTO)。 */
function ApprovalState({ ap }: { ap: ApprovalDetail }) {
  return (
    <section className="panel approval-box" data-testid={`approval-${ap.node_id}`}>
      <div className="panel-head">
        <span className="bn-badge bn-approval">审批 · {ap.mode === 'all' ? '会签' : '或签'}</span>
        <code className="panel-id">{ap.node_id}</code>
      </div>
      <p className="exec-meta">
        审批人:{ap.approvers?.join(', ') ?? '—'} · 待投:<span data-testid="pending-approvers">{ap.pending_approvers.join(', ') || '—'}</span>
      </p>
      {ap.decisions.length > 0 ? (
        <ul className="decisions">
          {ap.decisions.map((d) => (
            <li key={d.id} className="mono">
              {d.decided_at} · {d.decided_by} · {d.decision === 'approve' ? '批准' : '驳回'}
              {d.comment !== null && d.comment !== '' ? ` · ${d.comment}` : ''}
              {d.final === true ? ' · 终票' : ''}
            </li>
          ))}
        </ul>
      ) : (
        <p className="draft-empty">暂无投票</p>
      )}
    </section>
  )
}
