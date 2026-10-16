// 定义浏览只消费既有已发布/归档列表、versions、getVersion与archive端点。
// 全部版本已归档的名字仍可达；不把归档版本重新当成可运行版本。
import { useCallback, useEffect, useRef, useState } from 'react'
import { Archive, RefreshCw, TriangleAlert } from 'lucide-react'
import { ApiError, api, tokenStore, type DraftDetail, type DraftSummary } from './api.ts'
import { Select } from './ui/field.tsx'
import { Alert, Badge, Button, EmptyState, Panel, PanelHead } from './ui/primitives.tsx'
import { ConfirmLayer } from './ui/confirm.tsx'
import { Timestamp } from './ui/time.tsx'
import { apiErrorText } from './ui/errors.ts'
import { refreshConsole } from './refresh.ts'

export function PublishedVersions({ canManage, onAuthExpired }: { canManage: boolean; onAuthExpired: () => void }) {
  const [names, setNames] = useState<string[]>([])
  const [name, setName] = useState('')
  const [versions, setVersions] = useState<DraftSummary[]>([])
  const [selected, setSelected] = useState<DraftDetail | null>(null)
  const [namesLoading, setNamesLoading] = useState(true)
  const [versionsLoading, setVersionsLoading] = useState(false)
  const [reading, setReading] = useState(false)
  const [namesError, setNamesError] = useState('')
  const [versionsError, setVersionsError] = useState('')
  const [detailError, setDetailError] = useState('')
  const [actionError, setActionError] = useState('')
  const loading = namesLoading || versionsLoading
  const error = namesError || versionsError || detailError || actionError
  const [flash, setFlash] = useState('')
  const [target, setTarget] = useState<DraftSummary | null>(null)
  const [busy, setBusy] = useState(false)
  const listSeq = useRef(0)
  const versionSeq = useRef(0)
  const detailSeq = useRef(0)
  const archiveSeq = useRef(0)
  const inFlight = useRef(false)
  const layerRef = useRef<HTMLDivElement | null>(null)
  const opener = useRef<HTMLElement | null>(null)
  const wasArmed = useRef(false)
  useEffect(() => {
    if (wasArmed.current && target === null && opener.current?.isConnected) opener.current.focus()
    wasArmed.current = target !== null
  }, [target])

  const fail = useCallback((err: unknown, setError: (message: string) => void) => {
    if (err instanceof ApiError && err.status === 401) onAuthExpired()
    else setError(apiErrorText(err))
  }, [onAuthExpired])

  const loadNames = useCallback(async () => {
    const seq = ++listSeq.current
    setNamesLoading(true)
    setNamesError('')
    try {
      const [published, archived] = await Promise.all([api.listPublished(), api.listArchived()])
      if (seq !== listSeq.current) return
      const next = [...new Set([...published.items, ...archived.items].map((d) => d.name))].sort()
      setNames(next)
      setName((current) => next.includes(current) ? current : next[0] ?? '')
    } catch (err) { if (seq === listSeq.current) fail(err, setNamesError) }
    finally { if (seq === listSeq.current) setNamesLoading(false) }
  }, [fail])

  useEffect(() => {
    void loadNames()
    return () => { listSeq.current++; versionSeq.current++; detailSeq.current++; archiveSeq.current++ }
  }, [loadNames])

  const loadVersions = useCallback(async () => {
    const seq = ++versionSeq.current
    // 版本状态可被另端归档；刷新后不能让较早的详情响应复活旧状态。
    detailSeq.current++
    setReading(false)
    setDetailError('')
    if (name === '') { setVersions([]); setVersionsLoading(false); setVersionsError(''); return }
    setVersionsLoading(true)
    setVersionsError('')
    try {
      const data = await api.listVersions(name)
      if (seq !== versionSeq.current) return
      setVersions(data.items)
      setSelected((current) => {
        if (current === null) return null
        const row = data.items.find((item) => item.id === current.id)
        return row === undefined ? null : { ...current, ...row }
      })
    } catch (err) { if (seq === versionSeq.current) fail(err, setVersionsError) }
    finally { if (seq === versionSeq.current) setVersionsLoading(false) }
  }, [name, fail])
  useEffect(() => {
    detailSeq.current++
    setSelected(null)
    setReading(false)
    setDetailError('')
    setActionError('')
    setVersions([])
    setTarget(null)
    void loadVersions()
    return () => { versionSeq.current++; detailSeq.current++ }
  }, [loadVersions])

  const inspect = async (row: DraftSummary) => {
    if (row.version === null || loading || busy) return
    const seq = ++detailSeq.current
    setReading(true)
    setSelected(null)
    setDetailError('')
    try {
      const data = await api.getVersion(row.name, row.version)
      if (seq === detailSeq.current) setSelected(data)
    } catch (err) { if (seq === detailSeq.current) fail(err, setDetailError) }
    finally { if (seq === detailSeq.current) setReading(false) }
  }

  const archive = async () => {
    if (!canManage || loading || reading || error !== '' || target?.version == null || inFlight.current) return
    const row = target
    const seq = ++archiveSeq.current
    const requestToken = tokenStore.get()
    const current = () => seq === archiveSeq.current && tokenStore.get() === requestToken
    inFlight.current = true
    setBusy(true)
    setActionError('')
    try {
      const data = await api.archiveVersion(row.name, row.version!)
      if (!current()) return
      setVersions((current) => current.map((v) => v.id === data.id ? data : v))
      setSelected((current) => current?.id === data.id ? data : current)
      setFlash(`已归档 ${row.name} v${row.version}；已有执行不受影响。`)
      setTarget(null)
      refreshConsole()
    } catch (err) { if (current()) fail(err, setActionError) }
    finally { if (seq === archiveSeq.current) { inFlight.current = false; setBusy(false) } }
  }

  return <div className="min-h-0 flex-1 overflow-y-auto p-4" aria-label="已发布与版本历史">
    <Panel>
      <PanelHead title="已发布与版本历史" sub="包含已归档版本；定义只读" actions={<Button size="sm" icon={RefreshCw} disabled={loading || busy} onClick={() => { setActionError(''); void loadNames(); void loadVersions() }}>刷新版本</Button>} />
      <div className="panel-body flex flex-col gap-4" aria-busy={loading || reading}>
        {!canManage ? <p className="text-sm text-ink-2">当前角色可查看版本；编排、发布和归档需要 author/admin 角色。</p> : null}
        <Select label="工作流" value={name} disabled={loading || busy} onChange={(next) => { setFlash(''); setName(next) }}>{names.map((n) => <option key={n} value={n}>{n}</option>)}</Select>
        {loading ? <p role="status" className="text-sm text-ink-2">正在读取版本历史…</p> : null}
        {error !== '' ? <Alert tone="error" icon={TriangleAlert}><div><strong>版本操作未完成</strong><p>{error}</p></div></Alert> : null}
        {flash !== '' ? <p role="status" className="text-sm text-ink-1">{flash}</p> : null}
        {!loading && error === '' && names.length === 0 ? <EmptyState title="尚无已发布或归档版本" hint="发布草稿后，可在这里检查定义与版本历史。" /> : null}
        {versions.length > 0 ? <table className="table" aria-label={`${name} 版本历史`}>
          <thead><tr><th>版本</th><th>状态</th><th>更新时间</th><th>操作</th></tr></thead>
          <tbody>{versions.map((row) => <tr key={row.id} data-selected={selected?.id === row.id ? 'true' : 'false'}>
            <td className="num">v{row.version}</td><td><Badge>{row.status === 'ARCHIVED' ? '已归档' : row.status === 'PUBLISHED' ? '已发布' : row.status}</Badge></td>
            <td><Timestamp iso={row.updated_at} /></td>
            <td><div className="flex gap-2"><Button size="sm" variant="quiet" disabled={busy || loading} onClick={() => void inspect(row)}>查看 v{row.version} 定义</Button>
              {canManage && row.status === 'PUBLISHED' ? <Button size="sm" variant="quiet" icon={Archive} disabled={busy || loading || reading || error !== ''} onClick={(event) => { opener.current = event.currentTarget; setTarget(row) }}>归档 v{row.version}</Button> : null}</div></td>
          </tr>)}</tbody>
        </table> : null}
        {target !== null ? <ConfirmLayer layerRef={layerRef} label={`归档 ${target.name} v${target.version}`} hint="归档后不能再触发此版本，且不可恢复；已有执行和历史定义保留。" confirmText={`确认归档 v${target.version}`} busy={busy} confirmDisabled={!canManage || loading || reading || error !== ''} onConfirm={() => void archive()} onDismiss={() => { if (!inFlight.current) setTarget(null) }} /> : null}
        {reading ? <p role="status" className="text-sm text-ink-2">正在读取版本定义…</p> : null}
        {selected !== null ? <section aria-label="版本定义"><h2 className="text-md text-ink-0">{selected.name} v{selected.version} · {selected.status === 'ARCHIVED' ? '已归档' : '已发布'}</h2><pre className="mt-3 overflow-x-auto text-xs text-ink-1">{JSON.stringify(selected.dsl, null, 2)}</pre></section> : null}
      </div>
    </Panel>
  </div>
}
