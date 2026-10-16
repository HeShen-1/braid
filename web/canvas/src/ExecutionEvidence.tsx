// T23.9：仅消费既有证据读面。投影与轮次台账并非事务快照，缺失不补造。
import { useEffect, useRef, useState } from 'react'
import { ApiError, api, evidenceResponse, type AuditEvent, type NodeAttempt, type NodeAttemptSummary, type NodeProjection } from './api.ts'
import { Button, KV, Panel, PanelHead } from './ui/primitives.tsx'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'
import './styles/execution-evidence.css'

/** 原始 JSON 与 null 一同保留，不能把无记录解释为失败或零值。 */
export function EvidenceJSON({ value, testId }: { value: unknown; testId?: string }) {
  return <><p className="text-xs text-ink-2">解析后展示；大整数等精确数值请核对服务端响应原文。</p><pre className="exec-json evidence-json" data-testid={testId}>{value === undefined ? '未提供' : JSON.stringify(value, null, 2)}</pre></>
}

export function EvidenceRaw({ value, label = '服务端响应原文' }: { value: object; label?: string }) {
  return <RawText text={evidenceResponse(value)} label={label} />
}

function RawText({ text, label }: { text: string | undefined; label: string }) {
  return <details className="evidence-disclosure"><summary>{label}</summary><pre className="exec-json evidence-json">{text ?? '原文未保留，请刷新证据。'}</pre></details>
}

function readError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 404) return '暂无可用记录（可能未物化、已清理或超出范围）；可刷新重试。'
    if (err.status === 403) return `当前账号无权读取此证据（${err.code}）。`
    return `证据读取失败（${err.code}）；请重试。`
  }
  return '证据读取失败；请检查连接后重试。'
}

type Usage = Pick<NodeAttemptSummary, 'model' | 'prompt_tokens' | 'completion_tokens' | 'total_tokens' | 'cost_amount' | 'cost_currency'>

export function EvidenceUsage({ usage, estimated }: { usage: Usage; estimated: boolean }) {
  const count = (value: number | null) => value == null ? '未记录' : String(value)
  return (
    <div className="evidence-usage num">
      <span>模型 <code>{usage.model ?? '未记录'}</code></span>
      <span>输入 {count(usage.prompt_tokens)} · 输出 {count(usage.completion_tokens)} · 总计 {count(usage.total_tokens)} tokens</span>
      <span>
        成本 {usage.cost_amount == null ? '未记录' : String(usage.cost_amount)}
        {usage.cost_amount != null ? ` ${usage.cost_currency ?? '币种未记录'}` : usage.cost_currency != null ? ` · ${usage.cost_currency}` : ''}
        {estimated ? ' · 估算（estimated=true）' : ''}
      </span>
    </div>
  )
}

export function NodeEvidence({
  execId, nodeId, instanceNo, refreshKey, onAuthExpired, onClose,
}: {
  execId: string; nodeId: string; instanceNo: number; refreshKey: number
  onAuthExpired: () => void; onClose: () => void
}) {
  const [projection, setProjection] = useState<NodeProjection | null>(null)
  const [attempts, setAttempts] = useState<NodeAttemptSummary[]>([])
  const [attemptsRaw, setAttemptsRaw] = useState<string>()
  const [round, setRound] = useState<number | null>(null)
  const [attempt, setAttempt] = useState<NodeAttempt | null>(null)
  const [error, setError] = useState('')
  const [attemptError, setAttemptError] = useState('')
  const [loading, setLoading] = useState(true)
  const [retry, setRetry] = useState(0)
  const projectionReading = useRef(false)
  const attemptReading = useRef(false)

  useEffect(() => {
    let current = true
    projectionReading.current = true
    setLoading(true)
    setError('')
    void Promise.all([
      api.getNodeProjection(execId, nodeId, instanceNo),
      api.listNodeAttempts(execId, nodeId, instanceNo),
    ]).then(([p, rows]) => {
      if (!current) return
      setProjection(p)
      const items = Array.isArray(rows.items) ? rows.items : []
      setAttempts(items)
      setAttemptsRaw(evidenceResponse(rows))
      setRound((prev) => prev ?? items.at(-1)?.round ?? null)
    }).catch((err: unknown) => {
      if (!current) return
      if (err instanceof ApiError && err.status === 401) onAuthExpired()
      setError(readError(err))
    }).finally(() => { if (current) { projectionReading.current = false; setLoading(false) } })
    return () => { current = false; projectionReading.current = false }
  }, [execId, nodeId, instanceNo, retry, onAuthExpired])

  useEffect(() => {
    let current = true
    setAttempt(null)
    setAttemptError('')
    if (round !== null) {
      attemptReading.current = true
      void api.getNodeAttempt(execId, nodeId, instanceNo, round).then((value) => {
        if (current) setAttempt(value)
      }).catch((err: unknown) => {
        if (!current) return
        if (err instanceof ApiError && err.status === 401) onAuthExpired()
        setAttemptError(readError(err))
      }).finally(() => { if (current) attemptReading.current = false })
    }
    return () => { current = false; attemptReading.current = false }
  }, [execId, nodeId, instanceNo, round, retry, onAuthExpired])

  // 慢请求允许跨越多个执行轮询窗完成；poll不能取消它并造成饥饿。
  useEffect(() => {
    if (!projectionReading.current && !attemptReading.current) setRetry((n) => n + 1)
  }, [refreshKey])

  return (
    <section className="panel" data-testid={TID.nodeEvidence} aria-label={`节点证据 ${nodeId} #${instanceNo}`}>
      <PanelHead title="节点证据" sub={`${nodeId} #${instanceNo}`} actions={
        <><Button size="sm" disabled={loading} onClick={() => setRetry((n) => n + 1)}>刷新证据</Button><Button size="sm" variant="quiet" onClick={onClose}>收起证据</Button></>
      } />
      <div className="panel-body evidence-body">
        <p className="text-xs text-ink-2">轮次跨回滚递增；投影与台账分别读取，执行推进时可能短暂不同步。输入/输出为服务端 JSON 快照。</p>
        {loading ? <p role="status">正在读取节点证据…</p> : null}
        {error ? <p role="alert">{error}</p> : null}
        {projection !== null ? (
          <>
            <dl className="exec-kv">
              <KV k="当前投影"><code>{projection.status}</code> · attempts={projection.attempts} · attempts_base={projection.attempts_base}</KV>
              <KV k="轮次归属">历史轮次 ≤ {projection.attempts_base}；当前轮代 {projection.attempts_base + 1}…{projection.attempts > projection.attempts_base ? projection.attempts : '尚未开始'}</KV>
            </dl>
            <details className="evidence-disclosure">
              <summary>查看当前投影输入 / 输出</summary>
              <p className="text-xs text-ink-2">output 是节点成功轮投影；比较回滚前后请选下方的具体轮次。</p>
              <h3>输入 input_snapshot</h3><EvidenceJSON value={projection.input_snapshot} />
              <h3>输出 output</h3><EvidenceJSON value={projection.output} />
            </details>
            <EvidenceRaw value={projection} label="节点投影服务端响应原文" />
          </>
        ) : null}
        {!loading && !error && attempts.length === 0 ? <p>暂无轮次台账；可能未派发或记录已清理。</p> : null}
        {attempts.length > 0 ? (
          <div className="exec-table-wrap">
            <table className="table evidence-attempts">
              <thead><tr><th>轮次</th><th>状态 / attempt_id</th><th>本轮时长</th><th>用量 / 成本</th></tr></thead>
              <tbody>{attempts.map((a) => (
                <tr key={a.attempt_id} data-testid={tid.attemptRow(a.round)} aria-selected={round === a.round}>
                  <td><Button size="sm" variant="quiet" aria-pressed={round === a.round} onClick={() => setRound(a.round)}>第 {a.round} 轮</Button><span className="evidence-note">{projection ? a.round <= projection.attempts_base ? '历史轮次' : a.round <= projection.attempts ? '当前轮代' : '投影尚未同步' : '归属未读取'}</span></td>
                  <td><code>{a.status}</code><code className="evidence-note">{a.attempt_id}</code></td>
                  <td><AttemptDuration attempt={a} /></td>
                  <td><EvidenceUsage usage={a} estimated={a.estimated} /></td>
                </tr>
              ))}</tbody>
            </table>
          </div>
        ) : null}
        <RawText text={attemptsRaw} label="轮次列表服务端响应原文" />
        <p className="text-xs text-ink-2">本轮时长 = attempt 开始到结束的墙钟区间；挂起后的轮次间隔不计入，不等于 CPU / worker 纯运行计时。DEAD 缺少结束时刻时不推算时长。未记录的用量/成本不按 0 计算。</p>
        {round !== null ? (
          <section className="evidence-round" data-testid={TID.attemptEvidence} aria-label={`第 ${round} 轮证据`}>
            <h3>第 {round} 轮证据</h3>
            {attemptError ? <p role="alert">{attemptError}</p> : attempt === null ? <p role="status">正在读取本轮输入 / 输出…</p> : (
              <>
                <dl className="exec-kv">
                  <KV k="attempt_id"><code>{attempt.attempt_id}</code></KV>
                  <KV k="状态"><code>{attempt.status}</code></KV>
                  <KV k="开始"><Timestamp iso={attempt.started_at} relative={false} /></KV>
                  <KV k="结束"><Timestamp iso={attempt.finished_at} relative={false} /></KV>
                  <KV k="本轮时长"><AttemptDuration attempt={attempt} /></KV>
                  <KV k="错误"><code>{attempt.error_code ?? '未记录'}</code>{attempt.error_message != null ? ` · ${attempt.error_message}` : ''}</KV>
                </dl>
                <EvidenceUsage usage={attempt} estimated={attempt.estimated} />
                <h3>输入 input_snapshot</h3><EvidenceJSON value={attempt.input_snapshot} />
                <h3>输出 output</h3><EvidenceJSON value={attempt.output} />
                <EvidenceRaw value={attempt} label={`第 ${round} 轮服务端响应原文`} />
              </>
            )}
          </section>
        ) : null}
      </div>
    </section>
  )
}

function AttemptDuration({ attempt }: { attempt: NodeAttemptSummary }) {
  if (attempt.finished_at == null && attempt.status !== 'RUNNING') return <span>未知（未记录结束时刻）</span>
  return <Duration from={attempt.started_at} to={attempt.finished_at} />
}

// 展示层解释，不增加或替换后端事件词表。未知事件仍显示真实原码。
const EVENT_LABEL: Record<string, string> = {
  'execution.triggered': '执行已触发', 'execution.rolled_back': '执行已回滚',
  'execution.cancelled': '执行已取消', 'rollback.denied': '回滚被拒绝', 'cancel.denied': '取消被拒绝',
  'node.state_changed': '节点状态变化', 'node.redispatched': '节点重新派发',
  'node.deadline_extended': '节点期限延长', 'node.taint_propagated': '输入信任标记已传播',
  'attempt.reaped': '失联尝试已回收', 'branch.evaluated': '分支条件已求值',
  'llm.call.completed': '模型调用已完成', 'llm.call.discarded': '过期模型结果已丢弃',
  'llm.call.budget_denied': '模型调用预算不足', 'tool.call.completed': '工具调用已完成',
  'tool.call.denied': '工具调用被拒绝', 'approval.requested': '已请求审批',
  'approval.decided': '审批决定已提交', 'approval.decision_denied': '审批决定被拒绝',
  'approval.escalated': '审批已升级通知', 'approval.resume_redispatched': '审批续跑已重新派发',
  'approval.notified': '审批通知已发送', 'approval.im_notified': '审批即时消息已通知',
}

export function AuditEvidence({ executionId, traceId, partition, refreshKey, onAuthExpired, onTrace, onClose }: {
  executionId?: string; traceId?: string; partition?: string; refreshKey: number
  onAuthExpired: () => void; onTrace: (id: string) => void; onClose?: () => void
}) {
  const [items, setItems] = useState<AuditEvent[]>([])
  const [rawPages, setRawPages] = useState<string[]>([])
  const [cursor, setCursor] = useState('')
  const [pages, setPages] = useState(1)
  const [retry, setRetry] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const reading = useRef(false)

  // 刷新重读用户已展开的页数，避免回到首100条或拼出新旧游标混合页。
  useEffect(() => {
    let current = true
    reading.current = true
    setLoading(true)
    setError('')
    void (async () => {
      const rows = new Map<string, AuditEvent>()
      const cursors = new Set<string>()
      const raw: string[] = []
      let next = ''
      for (let page = 0; page < pages; page++) {
        const result = traceId !== undefined
          ? await api.listTraceAuditEvents(traceId, 100, next)
          : await api.listAuditEvents(executionId!, 100, next)
        if (!current) return
        raw.push(evidenceResponse(result) ?? '原文未保留，请刷新审计。')
        for (const row of result.items ?? []) rows.set(row.id, row)
        next = result.next_cursor ?? ''
        if (next === '') break
        if (cursors.has(next)) throw new Error('repeated_cursor')
        cursors.add(next)
      }
      if (current) { setItems([...rows.values()]); setCursor(next); setRawPages(raw) }
    })().catch((err: unknown) => {
      if (!current) return
      if (err instanceof ApiError && err.status === 401) onAuthExpired()
      setError(readError(err))
    }).finally(() => { if (current) { reading.current = false; setLoading(false) } })
    return () => { current = false; reading.current = false }
  }, [executionId, traceId, pages, retry, onAuthExpired])

  useEffect(() => {
    if (!reading.current) setRetry((n) => n + 1)
  }, [refreshKey])

  return (
    <Panel>
      <PanelHead title={traceId === undefined ? '审计事件' : 'Trace 审计'} sub={
        <span data-testid={traceId === undefined ? TID.auditCount : undefined}>已加载 {items.length} 条{cursor ? ' · 仍有更多' : loading || error ? '' : ' · 已到末页'}{partition ? ` · 分区 ${partition}` : ''}</span>
      } actions={<><Button size="sm" disabled={loading} onClick={() => setRetry((n) => n + 1)}>刷新审计</Button>{onClose ? <Button size="sm" variant="quiet" onClick={onClose}>收起 Trace</Button> : null}</>} />
      <div className="panel-body evidence-body">
        <p className="text-xs text-ink-2">{traceId !== undefined ? <><code>{traceId}</code> · 按当前账号授权范围查询此 trace 的事件。</> : 'append-only · 按事件时间倒序，异步审计可能稍后到达；已到末页仅指本次读取。'}</p>
        <p className="text-xs text-ink-2">trace_id 标识调用链；attempt_id 标识节点轮次。row_hash / prev_hash 是审计完整性链接，与 trace 不同；本页展示原值，不代表已验证哈希链。接口未返回 trace 父子链接，不推断父子关系。</p>
        {loading ? <p role="status">正在读取审计…</p> : null}
        {error ? <p role="alert">{error}{items.length > 0 ? ' 下方保留上次读取结果。' : ''}</p> : null}
        {rawPages.map((raw, i) => <RawText key={i} text={raw} label={`审计第 ${i + 1} 页服务端响应原文`} />)}
      </div>
      <div className="exec-table-wrap">
        <table className="table evidence-audit" data-testid={traceId === undefined ? TID.auditList : TID.traceAuditList}>
          <thead><tr><th>事件 / 原始证据</th><th>节点 / 轮次标识</th><th>用量 / 成本</th><th>主体 / 时间</th></tr></thead>
          <tbody>{items.length === 0 ? <tr><td colSpan={4}>{loading ? '正在读取…' : error ? '尚无可显示结果' : '本次查询暂无审计事件'}</td></tr> : items.map((ev) => (
            <tr key={ev.id} data-event-id={ev.id} data-testid={tid.auditRow(ev.event_type)}>
              <td>
                <details className="evidence-disclosure">
                  <summary><span>{EVENT_LABEL[ev.event_type] ?? '未收录解释的事件'}</span><code className="evidence-note">{ev.event_type}</code></summary>
                  <dl className="exec-kv">
                    <KV k="事件 ID"><code>{ev.id}</code></KV>
                    <KV k="row_hash"><code>{ev.row_hash ?? '未启链 / 未记录'}</code></KV>
                    <KV k="prev_hash"><code>{ev.prev_hash || '空（链首或存量未启链）'}</code></KV>
                    <KV k="PII 掩蔽">{ev.pii_masked ? '是' : '否'}</KV>
                    <KV k="落库时间"><Timestamp iso={ev.created_at} relative={false} /></KV>
                  </dl>
                  <h3>payload（解析后）</h3><EvidenceJSON value={ev.payload} />
                </details>
                <code className="evidence-note" title={ev.row_hash ?? undefined}>链 {ev.row_hash ? ev.row_hash.slice(0, 8) : '未记录'}</code>
              </td>
              <td><code>{ev.node_id ?? '—'}</code><code className="evidence-note">attempt_id {ev.attempt_id ?? '未记录'}</code>
                {ev.trace_id ? <Button size="sm" variant="quiet" className="evidence-trace" onClick={() => onTrace(ev.trace_id!)} title={ev.trace_id}>查询 trace <code>{ev.trace_id}</code></Button> : <span className="evidence-note">trace_id 未记录</span>}
              </td>
              <td><EvidenceUsage usage={ev} estimated={ev.cost_estimated} /></td>
              <td><span>{ev.actor_type}:{ev.actor_id}</span><span className="evidence-note"><Timestamp iso={ev.event_at} /></span></td>
            </tr>
          ))}</tbody>
        </table>
      </div>
      {cursor ? <div className="panel-body"><Button disabled={loading} data-testid={traceId === undefined ? TID.auditMore : undefined} onClick={() => setPages((n) => n + 1)}>加载更多审计</Button></div> : null}
    </Panel>
  )
}
