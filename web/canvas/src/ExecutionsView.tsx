// 执行面(T21.1 Lane B 重做)。
//
// 左栏 = 触发面板(已发布定义 + JSON 输入,幂等键每次提交新生成:双击收敛
// 为服务端重放)+ 执行表(状态筛选 + 工作流筛选 + 状态芯片 + 时长 + 创建);
// 右栏 = 选中执行的详情(ExecutionDetail,纵向分层 + 执行时间带)。
//
// 选中态由 URL 承载(routes/ExecutionsRoute:selectedId / onSelect 受控传入),
// 刷新与深链天然一致,组件不持有第二份选中真源。筛选是**服务端**过滤
// (api.listExecutions 既有 status/name/limit/offset 参数,零新端点),
// 换筛选 = 重置 offset 重拉。401 统一上抛 App 登出。
import { useCallback, useEffect, useState } from 'react'
import { Activity, FilterX, Inbox, Play, RefreshCw, TriangleAlert, X } from 'lucide-react'
import { ApiError, api, type DraftSummary, type ExecutionSummary } from './api.ts'
import { ExecutionDetail } from './ExecutionDetail.tsx'
import { Select, TextArea, TextInput } from './ui/field.tsx'
import { Alert, Button, EmptyState, PanelHead, StatusChip } from './ui/primitives.tsx'
import { EXEC_STATUS } from './ui/status.ts'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'

/** 非终态执行 → 列表自刷新(夜班盯盘:不点刷新也能看到新执行)。 */
const LIVE_LIST_MS = 5000
const LIVE_STATUSES = ['PENDING', 'RUNNING', 'WAITING_APPROVAL']

/** 一页 20 条:执行表是"扫"的不是"读"的,翻页比无限滚动更可控。 */
const PAGE_SIZE = 20

/** 名称筛选防抖:键盘每敲一下都发请求会把列表刷成噪声。 */
const NAME_DEBOUNCE_MS = 250

/** 状态词表复用执行域真源(不另抄一份);顺序 = DDL ENUM 序。 */
const STATUS_OPTIONS = Object.entries(EXEC_STATUS).map(([value, meta]) => ({ value, label: meta.label }))

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

  // 筛选:status/nameQuery 驱动请求;nameInput 是输入框现值(防抖后进 nameQuery)。
  const [status, setStatus] = useState('')
  const [nameInput, setNameInput] = useState('')
  const [nameQuery, setNameQuery] = useState('')
  const [offset, setOffset] = useState(0)
  const [total, setTotal] = useState(0)

  const filtered = status !== '' || nameQuery !== ''

  useEffect(() => {
    const t = window.setTimeout(() => setNameQuery(nameInput.trim()), NAME_DEBOUNCE_MS)
    return () => window.clearTimeout(t)
  }, [nameInput])

  const refresh = useCallback(async () => {
    try {
      const [ex, df] = await Promise.all([
        api.listExecutions({ status, name: nameQuery, limit: PAGE_SIZE, offset }),
        api.listPublished(),
      ])
      const items = Array.isArray(ex.items) ? ex.items : []
      setExecs(items)
      // page 是既有契约;缺字段时退化成"本页条数",不编总数。
      setTotal(typeof ex.page?.total === 'number' ? ex.page.total : items.length)
      setDefs(Array.isArray(df.items) ? df.items : [])
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthExpired()
    }
  }, [onAuthExpired, status, nameQuery, offset])

  useEffect(() => {
    void refresh()
  }, [refresh])

  // 筛选是一块"盯盘切片":带筛选时即使当前页没有活执行也保持自刷新
  // (否则筛 FAILED 时新失败永远不冒出来)。
  const hasLive = execs.some((e) => LIVE_STATUSES.includes(e.status))
  useEffect(() => {
    if (!hasLive && !filtered) return
    const t = window.setInterval(() => void refresh(), LIVE_LIST_MS)
    return () => window.clearInterval(t)
  }, [hasLive, filtered, refresh])

  /** 换筛选 = 回第一页(offset 是"上一次结果"的位置,不重置会读出空洞)。 */
  const changeFilter = useCallback((apply: () => void) => {
    apply()
    setOffset(0)
  }, [])

  const clearFilters = useCallback(() => {
    setStatus('')
    setNameInput('')
    setNameQuery('')
    setOffset(0)
  }, [])

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

  const showFilters = execs.length > 0 || filtered
  const showPager = execs.length > 0 && (total > PAGE_SIZE || offset > 0)

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
            sub={`${execs.length}${total !== execs.length ? ` / ${total}` : ''} 条`}
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
          {showFilters ? (
            <div className="exec-filters">
              {/* 不设"筛选"框标签:两个字段各自的标签已经说明了这一行是什么,
                  再加一个会让三个标签挤在两行里(收尾评审 R2)。 */}
              <Select
                label="按状态"
                value={status}
                onChange={(e) => changeFilter(() => setStatus(e.target.value))}
              >
                <option value="">全部状态</option>
                {STATUS_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
              <TextInput
                label="按名称"
                placeholder="工作流名称"
                value={nameInput}
                onChange={(e) => changeFilter(() => setNameInput(e.target.value))}
              />
              {/* 结果为空时「清除筛选」由空态承担,这里不再重复一个同名按钮 */}
              {filtered && execs.length > 0 ? (
                <Button variant="quiet" size="sm" icon={X} onClick={clearFilters}>
                  清除筛选
                </Button>
              ) : null}
            </div>
          ) : null}
          {execs.length === 0 ? (
            <div data-testid={TID.execEmpty}>
              {filtered ? (
                <EmptyState
                  icon={FilterX}
                  title="当前筛选下没有执行"
                  hint="没有匹配该状态与工作流名称的执行 —— 换个条件,或清除筛选查看全部。"
                  action={
                    <Button size="sm" icon={X} onClick={clearFilters}>
                      清除筛选
                    </Button>
                  }
                />
              ) : (
                <EmptyState icon={Inbox} title="暂无执行" hint="触发一次执行后,这里会出现执行记录与时间带。" />
              )}
            </div>
          ) : (
            <>
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
              {showPager ? (
                <div className="exec-pager">
                  <span className="num text-ink-2">
                    {offset + 1}–{offset + execs.length} / {total}
                  </span>
                  <Button
                    variant="quiet"
                    size="sm"
                    disabled={offset === 0}
                    onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                  >
                    上一页
                  </Button>
                  <Button
                    variant="quiet"
                    size="sm"
                    disabled={offset + execs.length >= total}
                    onClick={() => setOffset(offset + PAGE_SIZE)}
                  >
                    下一页
                  </Button>
                </div>
              ) : null}
            </>
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
