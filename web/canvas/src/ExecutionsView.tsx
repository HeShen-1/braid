// 执行面(T21.1 Lane B 重做)。
//
// 左栏 = 触发面板(已发布定义 + JSON 输入,幂等键每次提交新生成:双击收敛
// 为服务端重放)+ 执行表(状态芯片 + 工作流 + 时长 + 创建);右栏 = 选中执行
// 的详情(ExecutionDetail,纵向分层 + 执行时间带)。
//
// 选中态由 URL 承载(routes/ExecutionsRoute:selectedId / onSelect 受控传入),
// 刷新与深链天然一致,组件不持有第二份选中真源。401 统一上抛 App 登出。
import { useCallback, useEffect, useState } from 'react'
import { Activity, Inbox, Play, RefreshCw, TriangleAlert } from 'lucide-react'
import { ApiError, api, type DraftSummary, type ExecutionSummary } from './api.ts'
import { ExecutionDetail } from './ExecutionDetail.tsx'
import { Select, TextArea } from './ui/field.tsx'
import { Alert, Button, EmptyState, PanelHead, StatusChip } from './ui/primitives.tsx'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'

/** 非终态执行 → 列表自刷新(夜班盯盘:不点刷新也能看到新执行)。 */
const LIVE_LIST_MS = 5000
const LIVE_STATUSES = ['PENDING', 'RUNNING', 'WAITING_APPROVAL']

/** 幂等键:优先 randomUUID;非安全上下文(纯 HTTP 非 localhost 部署)
 * 无此 API——降级 getRandomValues 十六进制(该 API 不受 SecureContext
 * 限制;无降级时触发面板在 http://内网IP 形态整面失效)。 */
function freshIdempotencyKey(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const b = new Uint8Array(16)
  crypto.getRandomValues(b)
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
}

export function ExecutionsView({
  selectedId,
  onSelect,
  onAuthExpired,
}: {
  selectedId: string | null
  onSelect: (id: string | null) => void
  onAuthExpired: () => void
}) {
  const [execs, setExecs] = useState<ExecutionSummary[]>([])
  const [defs, setDefs] = useState<DraftSummary[]>([])
  const [defName, setDefName] = useState('')
  const [inputRaw, setInputRaw] = useState('{}')
  // 两个错误面分开:输入错误就近贴控件,接口错误贴按钮(不互相顶位)。
  const [inputError, setInputError] = useState('')
  const [apiError, setApiError] = useState('')
  const [busy, setBusy] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const [ex, df] = await Promise.all([api.listExecutions(), api.listPublished()])
      setExecs(Array.isArray(ex.items) ? ex.items : [])
      setDefs(Array.isArray(df.items) ? df.items : [])
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthExpired()
    }
  }, [onAuthExpired])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const hasLive = execs.some((e) => LIVE_STATUSES.includes(e.status))
  useEffect(() => {
    if (!hasLive) return
    const t = window.setInterval(() => void refresh(), LIVE_LIST_MS)
    return () => window.clearInterval(t)
  }, [hasLive, refresh])

  const trigger = useCallback(async () => {
    if (defName === '' || busy) return
    let input: unknown
    try {
      input = JSON.parse(inputRaw)
    } catch {
      setInputError('输入不是合法 JSON(须为对象)')
      return
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      setInputError('输入顶层必须是 JSON 对象')
      return
    }
    setBusy(true)
    setInputError('')
    setApiError('')
    try {
      const ack = await api.triggerExecution(defName, input, freshIdempotencyKey())
      await refresh()
      onSelect(ack.id)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthExpired()
        return
      }
      setApiError(err instanceof ApiError ? err.code : 'network_error')
    } finally {
      setBusy(false)
    }
  }, [defName, inputRaw, busy, refresh, onSelect, onAuthExpired])

  return (
    <div className="exec-view">
      <aside className="exec-rail" aria-label="执行列表与触发">
        <section className="panel" data-testid={TID.triggerPanel}>
          <PanelHead title="触发执行" sub={defs.length > 0 ? `${defs.length} 个已发布` : undefined} />
          <div className="panel-body exec-trigger">
            <Select
              label="已发布工作流"
              value={defName}
              data-testid={TID.triggerDefinition}
              onChange={(e) => setDefName(e.target.value)}
            >
              <option value="">选择已发布工作流…</option>
              {defs.map((d) => (
                <option key={d.id} value={d.name}>
                  {d.version !== null ? `${d.name} v${d.version}` : d.name}
                </option>
              ))}
            </Select>
            <TextArea
              label="输入 JSON"
              rows={4}
              value={inputRaw}
              error={inputError}
              hint="顶层必须是 JSON 对象;幂等键由界面每次提交新生成"
              data-testid={TID.triggerInput}
              onChange={(e) => setInputRaw(e.target.value)}
            />
            <div className="flex items-center gap-2">
              <Button
                variant="primary"
                icon={Play}
                data-testid={TID.triggerSubmit}
                disabled={defName === '' || busy}
                onClick={() => void trigger()}
              >
                触发执行
              </Button>
              {defName === '' ? <span className="text-xs text-ink-2">先选一个已发布工作流</span> : null}
            </div>
            {defs.length === 0 ? (
              <p className="field-hint">尚无已发布工作流 —— 先在工作流面发布一版,再回到这里触发。</p>
            ) : null}
            {apiError !== '' ? (
              <Alert tone="error" icon={TriangleAlert}>
                触发失败:<code className="mono ml-1">{apiError}</code>
              </Alert>
            ) : null}
          </div>
        </section>

        <section className="panel">
          <PanelHead
            title="执行"
            sub={`${execs.length} 条`}
            actions={
              <Button
                variant="quiet"
                size="sm"
                iconOnly
                icon={RefreshCw}
                aria-label="刷新执行列表"
                onClick={() => void refresh()}
              />
            }
          />
          {execs.length === 0 ? (
            <div data-testid={TID.execEmpty}>
              <EmptyState icon={Inbox} title="暂无执行" hint="触发一次执行后,这里会出现执行记录与时间带。" />
            </div>
          ) : (
            <div className="exec-table-wrap">
              <table className="table" data-testid={TID.execTable}>
                <thead>
                  <tr>
                    <th>状态</th>
                    <th>工作流</th>
                    <th>时长</th>
                    <th>创建</th>
                  </tr>
                </thead>
                <tbody>
                  {execs.map((e) => (
                    <tr
                      key={e.id}
                      data-testid={tid.execRow(e.id)}
                      data-state={e.status}
                      data-selected={selectedId === e.id ? 'true' : 'false'}
                      tabIndex={0}
                      title={`#${e.id} · ${e.definition_name} v${e.definition_version}`}
                      onClick={() => onSelect(e.id)}
                      onKeyDown={(ev) => {
                        if (ev.key === 'Enter' || ev.key === ' ') {
                          ev.preventDefault()
                          onSelect(e.id)
                        }
                      }}
                    >
                      <td>
                        <StatusChip status={e.status} />
                      </td>
                      <td>
                        <div className="exec-row-name">
                          <span className="truncate text-ink-0">{e.definition_name}</span>
                          <span className="num text-ink-2">v{e.definition_version}</span>
                        </div>
                      </td>
                      <td className="num">
                        <Duration from={e.started_at} to={e.finished_at} />
                      </td>
                      <td>
                        <Timestamp iso={e.created_at} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      </aside>

      {selectedId !== null ? (
        <ExecutionDetail key={selectedId} execId={selectedId} onAuthExpired={onAuthExpired} />
      ) : (
        <main className="exec-empty-main">
          <EmptyState
            icon={Activity}
            title="未选择执行"
            hint="从左侧列表选择一条执行,或在上方触发一次新执行 —— 详情会按真实时间铺开节点时间带。"
          />
        </main>
      )}
    </div>
  )
}
