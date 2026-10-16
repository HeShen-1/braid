// 执行面(T21.1 Lane B 重做)。
//
// 列表优先:状态页签 + 精确名称过滤 + 真实执行字段。未选择时触发面板在
// 侧栏;选择后保留执行表与详情,窄窗上下排列。触发目标与输入契约不变。
//
// 选中态由 URL 承载(routes/ExecutionsRoute:selectedId / onSelect 受控传入),
// 刷新与深链天然一致,组件不持有第二份选中真源。筛选是**服务端**过滤
// (api.listExecutions 既有 status/name/limit/offset 参数,零新端点),
// 换筛选 = 重置 offset 重拉。401 统一上抛 App 登出。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Activity, Check, FilterX, Inbox, Play, RefreshCw, TriangleAlert, X } from 'lucide-react'
import { ApiError, api, type DraftSummary, type ExecutionSummary } from './api.ts'
import { ExecutionDetail } from './ExecutionDetail.tsx'
import { Select, TextArea, TextInput } from './ui/field.tsx'
import { Alert, Button, EmptyState, PanelHead, StatusChip } from './ui/primitives.tsx'
import { EXEC_STATUS } from './ui/status.ts'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'
import type { DslInput } from './model/dsl.ts'
import { apiErrorText } from './ui/errors.ts'
import { refreshConsole } from './refresh.ts'
import { I } from './ui/I.tsx'

type InputContract = { definitionId: string; inputs: DslInput[]; error?: string }
const INPUT_EXAMPLES: Record<string, unknown> = { string: '示例', number: 0, boolean: false, object: {}, array: [] }

/** 非终态执行 → 列表自刷新(夜班盯盘:不点刷新也能看到新执行)。 */
const LIVE_LIST_MS = 5000
const LIVE_STATUSES = ['PENDING', 'RUNNING', 'WAITING_APPROVAL']

/** 一页 20 条:执行表是"扫"的不是"读"的,翻页比无限滚动更可控。 */
const PAGE_SIZE = 20

/** 名称筛选防抖:键盘每敲一下都发请求会把列表刷成噪声。 */
const NAME_DEBOUNCE_MS = 250

/** 状态词表复用执行域真源(不另抄一份);顺序 = DDL ENUM 序。 */
const STATUS_OPTIONS = [{ value: '', label: '全部执行' }, ...Object.entries(EXEC_STATUS).map(([value, meta]) => ({ value, label: meta.label }))]

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
  canManage = false,
}: {
  selectedId: string | null
  onSelect: (id: string | null) => void
  onAuthExpired: () => void
  canManage?: boolean
}) {
  const [execs, setExecs] = useState<ExecutionSummary[]>([])
  const [defs, setDefs] = useState<DraftSummary[]>([])
  const [listLoading, setListLoading] = useState(true)
  const [listError, setListError] = useState('')
  const [defsLoading, setDefsLoading] = useState(true)
  const [defsError, setDefsError] = useState('')
  const listSeq = useRef(0)
  const [selectedDef, setSelectedDef] = useState<DraftSummary | null>(null)
  // 初次只列每个工作流的最高已发布版本。选择后保留其快照,列表刷新发现
  // 新版/归档也不替换执行目标;旧选择保留在选项中,新版须主动重新选择。
  const latestDefs = useMemo(() => {
    const latest = new Map<string, DraftSummary>()
    for (const d of defs) {
      const seen = latest.get(d.name)
      if (seen === undefined || (d.version ?? 0) > (seen.version ?? 0)) latest.set(d.name, d)
    }
    return [...latest.values()]
  }, [defs])
  const defOptions = selectedDef !== null && !latestDefs.some((d) => d.id === selectedDef.id)
    ? [...latestDefs, selectedDef]
    : latestDefs
  const [inputRaw, setInputRaw] = useState('{}')
  // 两个错误面分开:输入错误就近贴控件,接口错误贴按钮(不互相顶位)。
  const [inputError, setInputError] = useState('')
  const [apiError, setApiError] = useState('')
  const [busy, setBusy] = useState(false)
  const [contract, setContract] = useState<InputContract | null>(null)
  const [contractRetry, setContractRetry] = useState(0)
  const currentContract = contract?.definitionId === selectedDef?.id ? contract : null
  const contractReady = currentContract !== null && currentContract.error === undefined

  useEffect(() => {
    if (selectedDef === null || selectedDef.version === null) return
    let active = true
    setContract(null)
    const target = `${selectedDef.name} v${selectedDef.version}`
    void api.getVersion(selectedDef.name, selectedDef.version).then((detail) => {
      if (!active) return
      setContract({
        definitionId: selectedDef.id,
        inputs: detail.dsl.inputs ?? [],
        ...(detail.status !== 'PUBLISHED' ? { error: `所选版本 ${target} 已归档或不可执行，请刷新后重新选择。` } : {}),
      })
    }).catch((err: unknown) => {
      if (!active) return
      if (err instanceof ApiError && err.status === 401) { onAuthExpired(); return }
      const error = err instanceof ApiError && err.status === 404
        ? `所选版本 ${target} 已失效或不存在，请刷新后重新选择。`
        : `读取 ${target} 的输入要求失败，请重试后再触发。`
      setContract({ definitionId: selectedDef.id, inputs: [], error })
    })
    return () => { active = false }
  }, [selectedDef, contractRetry, onAuthExpired])

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
    const seq = ++listSeq.current
    setListLoading(true)
    setDefsLoading(true)
    setListError('')
    setDefsError('')
    // 两个资源各自呈现结果；定义读取失败不能把已读到的执行装成空表。
    await Promise.all([
      api.listExecutions({ status, name: nameQuery, limit: PAGE_SIZE, offset }).then((ex) => {
        if (seq !== listSeq.current) return
        const items = Array.isArray(ex.items) ? ex.items : []
        setExecs(items)
        setTotal(typeof ex.page?.total === 'number' ? ex.page.total : items.length)
      }).catch((err: unknown) => {
        if (seq !== listSeq.current) return
        if (err instanceof ApiError && err.status === 401) onAuthExpired()
        else setListError(apiErrorText(err))
      }).finally(() => { if (seq === listSeq.current) setListLoading(false) }),
      api.listPublished().then((df) => {
        if (seq === listSeq.current) setDefs(Array.isArray(df.items) ? df.items : [])
      }).catch((err: unknown) => {
        if (seq !== listSeq.current) return
        if (err instanceof ApiError && err.status === 401) onAuthExpired()
        else setDefsError(apiErrorText(err))
      }).finally(() => { if (seq === listSeq.current) setDefsLoading(false) }),
    ])
  }, [onAuthExpired, status, nameQuery, offset])

  useEffect(() => {
    void refresh()
    return () => { listSeq.current++ }
  }, [refresh])

  // 筛选是一块"盯盘切片":带筛选时即使当前页没有活执行也保持自刷新
  // (否则筛 FAILED 时新失败永远不冒出来)。
  const hasLive = execs.some((e) => LIVE_STATUSES.includes(e.status))
  useEffect(() => {
    if (listLoading || defsLoading || listError !== '' || (!hasLive && !filtered)) return
    const t = window.setInterval(() => void refresh(), LIVE_LIST_MS)
    return () => window.clearInterval(t)
  }, [hasLive, filtered, listLoading, defsLoading, listError, refresh])

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
    if (!canManage || selectedDef === null || selectedDef.version === null || busy || !contractReady) return
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
    // required 指字段存在,不能把 false/0/空数组/空对象当成缺失。
    const missing = currentContract.inputs.filter((field) => field.required && !Object.prototype.hasOwnProperty.call(input, field.name))
    if (missing.length > 0) {
      setInputError(`缺少必填参数：${missing.map((field) => field.name).join('、')}。请在输入 JSON 中补齐。`)
      return
    }
    setBusy(true)
    setInputError('')
    setApiError('')
    try {
      const ack = await api.triggerExecution(selectedDef.name, input, freshIdempotencyKey(), selectedDef.version)
      refreshConsole()
      await refresh()
      onSelect(ack.id)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthExpired()
        return
      }
      const target = `${selectedDef.name} v${selectedDef.version}`
      if (err instanceof ApiError && (err.status === 404 || err.status === 409)) {
        const reason = err.status === 409 ? '已归档或不可执行' : '已失效或不存在'
        setApiError(`所选版本 ${target} ${reason}，请刷新后重新选择 (${err.code})`)
      } else {
        setApiError(apiErrorText(err))
      }
    } finally {
      setBusy(false)
    }
  }, [canManage, selectedDef, inputRaw, busy, contractReady, currentContract, refresh, onSelect, onAuthExpired])

  const showPager = execs.length > 0 && (total > PAGE_SIZE || offset > 0)

  return (
    <div className="exec-view" data-detail={selectedId !== null}>
      <aside className="exec-rail" aria-label="执行列表与触发">
        <section className="exec-list" aria-label="执行列表" aria-busy={listLoading}>
          <PanelHead
            title="执行记录"
            sub={listLoading ? '加载中…' : listError !== '' ? '读取失败' : `${execs.length}${total !== execs.length ? ` / ${total}` : ''} 条`}
            actions={
              <Button
                variant="quiet"
                size="sm"
                icon={RefreshCw}
                aria-label="刷新执行列表"
                disabled={listLoading}
                onClick={() => void refresh()}
              >刷新</Button>
            }
          />
          <div className="exec-status-tabs" role="tablist" aria-label="执行状态视图">
            {STATUS_OPTIONS.map((option, index) => (
              <button
                key={option.value}
                type="button"
                role="tab"
                className="tab"
                id={`exec-status-${option.value || 'all'}`}
                aria-controls="exec-list-results"
                aria-selected={status === option.value}
                tabIndex={status === option.value ? 0 : -1}
                onClick={() => changeFilter(() => setStatus(option.value))}
                onKeyDown={(event) => {
                  let next = index
                  if (event.key === 'ArrowRight') next = (index + 1) % STATUS_OPTIONS.length
                  else if (event.key === 'ArrowLeft') next = (index + STATUS_OPTIONS.length - 1) % STATUS_OPTIONS.length
                  else if (event.key === 'Home') next = 0
                  else if (event.key === 'End') next = STATUS_OPTIONS.length - 1
                  else return
                  event.preventDefault()
                  changeFilter(() => setStatus(STATUS_OPTIONS[next].value))
                  event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus()
                }}
              >{option.label}</button>
            ))}
          </div>
          <div className="exec-filters">
              <TextInput
                label="按名称"
                placeholder="完整工作流名称"
                hint="精确匹配，须输入完整名称"
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
          <div id="exec-list-results" role="tabpanel" aria-labelledby={`exec-status-${status || 'all'}`}>
          {listLoading ? <p className="panel-body text-sm text-ink-2" role="status">正在加载执行列表…</p> : listError !== '' ? (
            <Alert tone="error" icon={TriangleAlert}>
              <div><strong>执行列表读取失败</strong><p>{listError}</p><Button size="sm" onClick={() => void refresh()}>重试执行列表</Button></div>
            </Alert>
          ) : execs.length === 0 ? (
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
                <table className="table exec-table" role="grid" aria-label="执行列表" data-testid={TID.execTable}>
                  <thead>
                    <tr>
                      <th scope="col">执行 ID</th>
                      <th scope="col">工作流 / 版本</th>
                      <th scope="col">状态</th>
                      <th scope="col">时长</th>
                      <th scope="col">创建</th>
                    </tr>
                  </thead>
                  <tbody>
                    {execs.map((e) => (
                      <tr
                        key={e.id}
                        data-testid={tid.execRow(e.id)}
                        data-state={e.status}
                        data-selected={selectedId === e.id ? 'true' : 'false'}
                        aria-selected={selectedId === e.id}
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
                        <td className="exec-id-cell">
                          <span className="exec-selection-mark" aria-hidden>{selectedId === e.id ? <I glyph={Check} size={12} /> : null}</span>
                          <code className="exec-row-id" title={`#${e.id}`}>#{e.id}</code>
                        </td>
                        <td>
                          <div className="exec-row-name">
                            <span className="truncate text-ink-0">{e.definition_name}</span>
                            <span className="num text-ink-2">v{e.definition_version}</span>
                          </div>
                        </td>
                        <td>
                          <StatusChip status={e.status} />
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
          </div>
          <p className="exec-table-hint"><kbd>Enter</kbd> / <kbd>Space</kbd> 查看所选执行的节点与审计</p>
        </section>

        {canManage ? <section className="panel exec-trigger-panel" data-testid={TID.triggerPanel}>
          <PanelHead title="触发执行" sub={latestDefs.length > 0 ? `${latestDefs.length} 个已发布` : undefined} />
          <div className="panel-body exec-trigger">
            <Select
              label="已发布工作流"
              value={selectedDef?.id ?? ''}
              disabled={busy || defsLoading || defsError !== ''}
              data-testid={TID.triggerDefinition}
              hint={selectedDef === null ? '按所选版本执行；发布新版本不会替换当前选择' : selectedDef.version === null ? '该版本无有效版本号，请刷新后重新选择' : `执行目标：${selectedDef.name} v${selectedDef.version}`}
              onChange={(id) => {
                const next = defOptions.find((d) => d.id === id)
                // 移除先前保留的选项时底层可能发空值通知,不抹去刚选的新目标。
                if (next === undefined) return
                setSelectedDef(next)
                setApiError('')
                setInputError('')
              }}
            >
              <option value="" disabled>选择已发布工作流…</option>
              {defOptions.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.version !== null ? `${d.name} v${d.version}` : d.name}
                </option>
              ))}
            </Select>
            {selectedDef !== null && selectedDef.version !== null ? (
              <div className="trigger-contract" aria-label="所选版本输入要求" aria-live="polite">
                {currentContract === null ? <p className="field-hint" role="status">正在读取 {selectedDef.name} v{selectedDef.version} 的输入要求…</p>
                  : currentContract.error !== undefined ? (
                    <>
                      <p className="field-error" role="alert">{currentContract.error}</p>
                      <Button size="sm" variant="quiet" onClick={() => { setContract(null); setContractRetry((v) => v + 1) }}>重试读取输入要求</Button>
                    </>
                  ) : currentContract.inputs.length === 0 ? <p className="field-hint">此版本未声明触发参数，可提交空对象 {'{}'}。</p> : (
                    <>
                      <p className="field-label">输入要求 · {selectedDef.name} v{selectedDef.version}</p>
                      <ul className="trigger-inputs">
                        {currentContract.inputs.map((field) => (
                          <li key={field.name}>
                            <code className="mono">{field.name}</code>
                            <span>{field.type} · {field.required ? '必填' : '选填'}</span>
                            {field.description ? <p className="field-hint">{field.description}</p> : null}
                          </li>
                        ))}
                      </ul>
                      <details className="trigger-example">
                        <summary>输入 JSON 示例</summary>
                        <pre>{JSON.stringify(Object.fromEntries(currentContract.inputs.map((field) => [field.name, INPUT_EXAMPLES[field.type]])), null, 2)}</pre>
                      </details>
                    </>
                  )}
              </div>
            ) : null}
            <TextArea
              label="输入 JSON"
              rows={4}
              value={inputRaw}
              error={inputError}
              hint="顶层必须是 JSON 对象;幂等键由界面每次提交新生成"
              data-testid={TID.triggerInput}
              onChange={(e) => { setInputRaw(e.target.value); setInputError('') }}
            />
            <div className="flex items-center gap-2">
              <Button
                variant="primary"
                icon={Play}
                data-testid={TID.triggerSubmit}
                disabled={selectedDef === null || selectedDef.version === null || busy || !contractReady || defsLoading || defsError !== ''}
                onClick={() => void trigger()}
              >
                {busy ? '正在触发…' : '触发执行'}
              </Button>
              {selectedDef === null && defs.length > 0 ? <span className="text-xs text-ink-2">先选一个已发布工作流</span> : null}
            </div>
            {defsLoading ? <p className="field-hint" role="status">正在读取已发布工作流…</p> : null}
            {defsError !== '' ? (
              <Alert tone="error" icon={TriangleAlert}>
                <div><strong>已发布工作流读取失败</strong><p>{defsError}</p><Button size="sm" onClick={() => void refresh()}>重试已发布工作流</Button></div>
              </Alert>
            ) : null}
            {defs.length === 0 && !defsLoading && defsError === '' ? (
              <p className="field-hint">尚无已发布工作流，请先在工作流页发布一版，再回到这里触发。</p>
            ) : null}
            {apiError !== '' ? (
              <Alert tone="error" icon={TriangleAlert}>
                触发失败：{apiError}
              </Alert>
            ) : null}
          </div>
        </section> : <p className="exec-permission text-sm text-ink-2">当前角色可查看执行；触发、回滚、续跑和取消需要 author/admin 角色。</p>}
      </aside>

      {selectedId !== null ? (
          <ExecutionDetail key={selectedId} execId={selectedId} onAuthExpired={onAuthExpired} canManage={canManage} onBack={() => onSelect(null)} />
      ) : (
        <p className="exec-selection-hint"><I glyph={Activity} /><strong>未选择执行</strong><span>从执行记录选择一行，查看节点时间带与完整审计。</span></p>
      )}
    </div>
  )
}
