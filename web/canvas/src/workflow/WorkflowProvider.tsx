// 工作流工作区状态。**挂在壳层而非路由页上**——切到执行面再回来时,
// 未保存的画布编辑必须还在(对抗审计 MEDIUM:条件卸载静默清空编辑态)。
// 文档真相在这里,CanvasEditor 只是它的编辑器。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ApiError, api, type DraftSummary, type Issue } from '../api.ts'
import { useAuth } from '../auth.tsx'
import { blankDoc } from '../defaults.ts'
import { fromCanvas, serializeDoc, toCanvas } from '../model/convert.ts'
import type { WorkflowDoc } from '../model/dsl.ts'
import { apiErrorText } from '../ui/errors.ts'

interface Workspace {
  key: number
  draftId: string | null
  doc: WorkflowDoc
  saved: string | null
}

// 首开时自动布局/补画布元数据不算作者修改;比较同一画布表示,
// 避免只选中节点也被误报为未保存。
function snapshot(doc: WorkflowDoc): string {
  return serializeDoc(fromCanvas(toCanvas(doc), doc))
}

function isDirty(ws: Workspace | null): boolean {
  return ws !== null && (ws.saved === null || snapshot(ws.doc) !== ws.saved)
}

interface WorkflowValue {
  drafts: DraftSummary[]
  loading: boolean
  listError: string
  ws: Workspace | null
  dirty: boolean
  issues: Issue[]
  flash: string
  setFlash: (s: string) => void
  editorKey: number
  refresh: () => Promise<void>
  open: (id: string) => Promise<boolean>
  newLocal: () => void
  close: () => void
  shouldConfirmLeave: (pathname: string) => boolean
  /** 编辑器每次变更上报——离开路由不丢编辑。 */
  onDocChange: (doc: WorkflowDoc) => void
  save: (doc: WorkflowDoc) => Promise<void>
  /** true = 已发布且工作区关闭;false = 失败或仍有后续编辑需保留。 */
  publish: (doc: WorkflowDoc) => Promise<boolean>
}

const Ctx = createContext<WorkflowValue | null>(null)

export function WorkflowProvider({ children }: { children: ReactNode }) {
  const { user, logout, canAuthor } = useAuth()
  const [drafts, setDrafts] = useState<DraftSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [listError, setListError] = useState('')
  const listSeq = useRef(0)
  const [ws, setWs] = useState<Workspace | null>(null)
  const [issues, setIssues] = useState<Issue[]>([])
  const [flash, setFlash] = useState('')
  const [editorKey, setEditorKey] = useState(0)
  const wsRef = useRef<Workspace | null>(null)
  const nextKey = useRef(0)
  const openSeq = useRef(0)
  const inFlight = useRef(new Set<number>())
  const setWorkspace = useCallback((next: Workspace | null) => {
    wsRef.current = next
    setWs(next)
  }, [])
  const dirty = isDirty(ws)

  const shouldConfirmLeave = useCallback((pathname: string) => {
    const cur = wsRef.current
    const currentPath = cur?.draftId == null ? '/workflows' : `/workflows/${cur.draftId}`
    return pathname !== currentPath && isDirty(cur)
  }, [])

  useEffect(() => {
    if (!dirty) return
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (!isDirty(wsRef.current)) return
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [dirty])

  const guard = useCallback(
    (err: unknown) => {
      if (err instanceof ApiError && err.status === 401) {
        logout()
        return true
      }
      return false
    },
    [logout],
  )

  const refresh = useCallback(async () => {
    const seq = ++listSeq.current
    setLoading(true)
    setListError('')
    try {
      const { items } = await api.listDrafts()
      if (seq !== listSeq.current) return
      setDrafts(items)
    } catch (err) {
      if (seq !== listSeq.current) return
      if (!guard(err)) setListError(apiErrorText(err))
    } finally {
      if (seq === listSeq.current) setLoading(false)
    }
  }, [guard])

  useEffect(() => {
    if (user !== null) void refresh()
    else {
      listSeq.current++
      setDrafts([])
      setListError('')
      setLoading(false)
    }
  }, [user, refresh])

  const open = useCallback(
    async (id: string) => {
      if (!canAuthor) return false
      const seq = ++openSeq.current
      if (wsRef.current?.draftId === id) return true
      const before = wsRef.current
      const beforeDoc = before === null ? null : snapshot(before.doc)
      try {
        const detail = await api.getDraft(id)
        // 工作区只编草稿(F04):已发布/归档的 definition 不复活编辑器 ——
        // 否则发布后深链回弹会把刚定格的文档重新 open 成僵尸态,还会顺手
        // 清掉发布成功条。查看已发布走执行面。
        const cur = wsRef.current
        if (seq !== openSeq.current || cur?.key !== before?.key || (cur !== null && snapshot(cur.doc) !== beforeDoc)) return false
        if (detail.status !== 'DRAFT') return false
        setIssues([])
        setFlash('')
        const key = ++nextKey.current
        setWorkspace({ key, draftId: id, doc: detail.dsl, saved: snapshot(detail.dsl) })
        setEditorKey(key)
        return true
      } catch (err) {
        if (guard(err) || seq !== openSeq.current) return false
        setIssues([{ code: err instanceof ApiError ? err.code : 'network_error', detail: apiErrorText(err) }])
        return false
      }
    },
    [guard, setWorkspace, canAuthor],
  )

  const newLocal = useCallback(() => {
    if (!canAuthor) return
    openSeq.current++
    setIssues([])
    setFlash('')
    const key = ++nextKey.current
    setWorkspace({ key, draftId: null, doc: blankDoc(), saved: null })
    setEditorKey(key)
  }, [setWorkspace, canAuthor])

  const close = useCallback(() => {
    openSeq.current++
    setWorkspace(null)
    setIssues([])
    setFlash('')
  }, [setWorkspace])

  const onDocChange = useCallback((doc: WorkflowDoc) => {
    if (!canAuthor) return
    // F05:旧校验错误描述的是编辑前的文档,一改即过期——就地清除,
    // 不再常驻到下次保存(错误横幅只在「刚失败」的窗口内可见)。
    setIssues((cur) => (cur.length > 0 ? [] : cur))
    const cur = wsRef.current
    if (cur === null) return
    if (snapshot(cur.doc) !== snapshot(doc)) setFlash('')
    setWorkspace({ ...cur, doc })
  }, [setWorkspace, canAuthor])

  // 保存 = create-or-update:首次(本地新建)POST /definitions,之后 PUT。
  const saveDoc = useCallback(
    async (doc: WorkflowDoc) => {
      if (!canAuthor) return null
      const cur = wsRef.current
      if (cur === null) return null
      const saved = snapshot(doc)
      setWorkspace({ ...cur, doc })
      try {
        const detail = cur.draftId === null
          ? await api.createDraft(serializeDoc(doc))
          : await api.updateDraft(cur.draftId, serializeDoc(doc))
        void refresh()
        const latest = wsRef.current
        if (latest?.key !== cur.key) return null
        // 在飞 create 不能把后来改过名称的本地文档锁到旧名称的服务端 id。
        if (latest.doc.name !== doc.name) return null
        setWorkspace({ ...latest, draftId: detail.id, saved })
        if (snapshot(latest.doc) === saved) {
          setFlash(cur.draftId === null ? `已创建 ${detail.name}` : '已保存')
          setIssues([])
        }
        return { id: detail.id, key: cur.key, saved }
      } catch (err) {
        if (guard(err) || wsRef.current?.key !== cur.key) return null
        if (snapshot(wsRef.current.doc) !== saved) return null
        if (err instanceof ApiError) {
          setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: apiErrorText(err) }])
        } else {
          setIssues([{ code: 'network_error', detail: '网络异常，保存未完成。' }])
        }
        setFlash('')
        return null
      }
    },
    [guard, refresh, setWorkspace, canAuthor],
  )

  const save = useCallback(async (doc: WorkflowDoc) => {
    const cur = wsRef.current
    if (cur === null || inFlight.current.has(cur.key)) return
    inFlight.current.add(cur.key)
    try {
      await saveDoc(doc)
    } finally {
      inFlight.current.delete(cur.key)
    }
  }, [saveDoc])

  const publish = useCallback(async (doc: WorkflowDoc): Promise<boolean> => {
    const cur = wsRef.current
    if (cur === null || cur.draftId === null || inFlight.current.has(cur.key)) return false
    inFlight.current.add(cur.key)
    try {
      const saved = await saveDoc(doc)
      if (saved === null) return false
      if (wsRef.current?.key !== saved.key) return false
      if (snapshot(wsRef.current.doc) !== saved.saved) {
        setFlash('保存期间画布已修改，请再次发布。')
        return false
      }
      const detail = await api.publishDraft(saved.id)
      void refresh()
      const latest = wsRef.current
      if (latest?.key !== saved.key) return false
      const message = `已发布 ${detail.name}${detail.version !== null ? ` v${detail.version}` : ''}`
      if (snapshot(latest.doc) !== saved.saved) {
        // 发布请求已经发出时无法撤回,后续编辑作为本地草稿保留,
        // 不把它继续绑到已经定格的 PUBLISHED definition。
        setWorkspace({ ...latest, draftId: null, saved: null })
        setFlash(`${message}；后续更改尚未保存。`)
        return false
      }
      // 发布后草稿定格为 PUBLISHED:退出编辑器,避免僵尸态(后续保存/发布
      // 只会撞 409,被误读成校验未过)。成功反馈由路由层的列表态成功条承接
      // (编辑器已卸载,F04);深链清理由路由层做 —— 本 provider 不持
      // router,裸挂可测;深链回弹即使抢跑,open() 对非 DRAFT 的拒绝也
      // 兜底(不复活编辑器、不清 flash)。
      setWorkspace(null)
      setFlash(message)
      setIssues([])
      return true
    } catch (err) {
      if (guard(err) || wsRef.current?.key !== cur.key) return false
      // 发布有独立白名单校验;请求之后编辑/删除的节点不能再挂旧 ref 错误。
      if (err instanceof ApiError && err.code === 'invalid_dsl' && snapshot(wsRef.current.doc) !== snapshot(doc)) {
        setIssues([])
        setFlash('先前内容发布失败；画布已修改，请再次发布。')
        return false
      }
      if (err instanceof ApiError) {
        setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: apiErrorText(err) }])
      } else {
        setIssues([{ code: 'network_error', detail: '网络异常，发布未完成。' }])
      }
      setFlash('')
      return false
    } finally {
      inFlight.current.delete(cur.key)
    }
  }, [guard, refresh, saveDoc, setWorkspace])

  const value = useMemo<WorkflowValue>(
    () => ({ drafts, loading, listError, ws, dirty, issues, flash, setFlash, editorKey, refresh, open, newLocal, close, shouldConfirmLeave, onDocChange, save, publish }),
    [drafts, loading, listError, ws, dirty, issues, flash, editorKey, refresh, open, newLocal, close, shouldConfirmLeave, onDocChange, save, publish],
  )

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useWorkflow(): WorkflowValue {
  const v = useContext(Ctx)
  if (v === null) throw new Error('useWorkflow 必须在 WorkflowProvider 内使用')
  return v
}
