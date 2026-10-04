// 执行视图(T16.6):触发面板(已发布定义 + 输入 JSON,幂等键每次提交
// 新生成——双击收敛为重放)+ 执行列表 + 执行详情。401 上抛 App 登出。
import { useCallback, useEffect, useState } from 'react'
import { ApiError, api, type DraftSummary, type ExecutionSummary } from './api.ts'
import { ExecutionDetail } from './ExecutionDetail.tsx'

export function ExecutionsView({ onAuthExpired }: { onAuthExpired: () => void }) {
  const [execs, setExecs] = useState<ExecutionSummary[]>([])
  const [defs, setDefs] = useState<DraftSummary[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [defName, setDefName] = useState('')
  const [inputRaw, setInputRaw] = useState('{}')
  const [triggerError, setTriggerError] = useState('')
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

  const trigger = useCallback(async () => {
    if (defName === '' || busy) return
    let input: unknown
    try {
      input = JSON.parse(inputRaw)
    } catch {
      setTriggerError('输入不是合法 JSON(须为对象)')
      return
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      setTriggerError('输入顶层必须是 JSON 对象')
      return
    }
    setBusy(true)
    setTriggerError('')
    try {
      const ack = await api.triggerExecution(defName, input, crypto.randomUUID())
      setSelected(ack.id)
      await refresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthExpired()
        return
      }
      setTriggerError(err instanceof ApiError ? err.code : 'network_error')
    } finally {
      setBusy(false)
    }
  }, [defName, inputRaw, busy, refresh, onAuthExpired])

  return (
    <div className="canvas-workspace exec-layout">
      <nav className="exec-side" aria-label="执行视图侧栏">
        <section className="panel" data-testid="trigger-panel">
          <div className="panel-head">
            <span className="bn-badge">触发</span>
          </div>
          <select
            className="f-select"
            data-testid="trigger-definition"
            value={defName}
            onChange={(e) => setDefName(e.target.value)}
          >
            <option value="">选择已发布工作流…</option>
            {defs.map((d) => (
              <option key={d.id} value={d.name}>
                {d.name}
                {d.version !== null ? ` v${d.version}` : ''}
              </option>
            ))}
          </select>
          <textarea
            className="f-area"
            rows={3}
            data-testid="trigger-input"
            aria-label="触发输入 JSON"
            value={inputRaw}
            onChange={(e) => setInputRaw(e.target.value)}
          />
          <button type="button" className="f-add" data-testid="trigger-submit" disabled={defName === '' || busy} onClick={() => void trigger()}>
            触发执行
          </button>
          {triggerError !== '' ? (
            <p className="error" role="alert" data-testid="trigger-error">
              {triggerError}
            </p>
          ) : null}
        </section>

        <div className="exec-list" data-testid="exec-list">
          <div className="bar-spacer">
            <span>执行(近 {execs.length})</span>
            <button type="button" className="f-add" data-testid="exec-refresh" onClick={() => void refresh()}>
              刷新
            </button>
          </div>
          {execs.length === 0 ? <p className="draft-empty">暂无执行</p> : null}
          <ul>
            {execs.map((e) => (
              <li key={e.id}>
                <button
                  type="button"
                  className={`draft-item${selected === e.id ? ' active' : ''}`}
                  data-testid={`exec-row-${e.id}`}
                  onClick={() => setSelected(e.id)}
                >
                  <span className="draft-name">
                    {e.definition_name} v{e.definition_version}
                  </span>
                  <span className={`exec-status st-${e.status}`}>{e.status}</span>
                  <span className="draft-time">{e.created_at}</span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      </nav>

      {selected !== null ? (
        <ExecutionDetail key={selected} execId={selected} onAuthExpired={onAuthExpired} />
      ) : (
        <main className="canvas-main">
          <p>从左侧选择执行,或触发一次新执行</p>
        </main>
      )}
    </div>
  )
}
