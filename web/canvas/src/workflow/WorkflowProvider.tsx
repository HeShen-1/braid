// 工作流工作区状态。**挂在壳层而非路由页上**——切到执行面再回来时,
// 未保存的画布编辑必须还在(对抗审计 MEDIUM:条件卸载静默清空编辑态)。
// 文档真相在这里,CanvasEditor 只是它的编辑器。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ApiError, api, type DraftSummary, type Issue } from '../api.ts'
import { useAuth } from '../auth.tsx'
import { blankDoc } from '../defaults.ts'
import { serializeDoc } from '../model/convert.ts'
import type { WorkflowDoc } from '../model/dsl.ts'

interface Workspace {
  draftId: string | null
  doc: WorkflowDoc
}

interface WorkflowValue {
  drafts: DraftSummary[]
  loading: boolean
  ws: Workspace | null
  issues: Issue[]
  flash: string
  setFlash: (s: string) => void
  editorKey: number
  refresh: () => Promise<void>
  open: (id: string) => Promise<void>
  newLocal: () => void
  close: () => void
  /** 编辑器每次变更上报——离开路由不丢编辑。 */
  onDocChange: (doc: WorkflowDoc) => void
  save: (doc: WorkflowDoc) => Promise<void>
  publish: () => Promise<void>
}

const Ctx = createContext<WorkflowValue | null>(null)

export function WorkflowProvider({ children }: { children: ReactNode }) {
  const { user, logout } = useAuth()
  const [drafts, setDrafts] = useState<DraftSummary[]>([])
  const [loading, setLoading] = useState(false)
  const [ws, setWs] = useState<Workspace | null>(null)
  const [issues, setIssues] = useState<Issue[]>([])
  const [flash, setFlash] = useState('')
  const [editorKey, setEditorKey] = useState(0)
  const wsRef = useRef<Workspace | null>(null)
  wsRef.current = ws

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
    setLoading(true)
    try {
      const { items } = await api.listDrafts()
      setDrafts(items)
    } catch (err) {
      if (!guard(err)) setDrafts([])
    } finally {
      setLoading(false)
    }
  }, [guard])

  useEffect(() => {
    if (user !== null) void refresh()
    else setDrafts([])
  }, [user, refresh])

  const open = useCallback(
    async (id: string) => {
      if (wsRef.current?.draftId === id) return
      try {
        const detail = await api.getDraft(id)
        setIssues([])
        setFlash('')
        setWs({ draftId: id, doc: detail.dsl })
        setEditorKey((k) => k + 1)
      } catch (err) {
        if (guard(err)) return
        setIssues([{ code: 'network_error', detail: '打开工作流失败，请稍后重试。' }])
      }
    },
    [guard],
  )

  const newLocal = useCallback(() => {
    setIssues([])
    setFlash('')
    setWs({ draftId: null, doc: blankDoc() })
    setEditorKey((k) => k + 1)
  }, [])

  const close = useCallback(() => {
    setWs(null)
    setIssues([])
    setFlash('')
  }, [])

  const onDocChange = useCallback((doc: WorkflowDoc) => {
    setWs((cur) => (cur === null ? cur : { ...cur, doc }))
  }, [])

  // 保存 = create-or-update:首次(本地新建)POST /definitions,之后 PUT。
  const save = useCallback(
    async (doc: WorkflowDoc) => {
      const cur = wsRef.current
      if (cur === null) return
      try {
        if (cur.draftId === null) {
          const detail = await api.createDraft(serializeDoc(doc))
          setWs({ draftId: detail.id, doc })
          setFlash(`已创建 ${detail.name}`)
        } else {
          await api.updateDraft(cur.draftId, serializeDoc(doc))
          setWs({ ...cur, doc })
          setFlash('已保存')
        }
        setIssues([])
        void refresh()
      } catch (err) {
        if (guard(err)) return
        if (err instanceof ApiError) {
          setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: err.code }])
        } else {
          setIssues([{ code: 'network_error', detail: '网络异常，保存未完成。' }])
        }
        setFlash('')
      }
    },
    [guard, refresh],
  )

  const publish = useCallback(async () => {
    const cur = wsRef.current
    if (cur === null || cur.draftId === null) return
    try {
      const detail = await api.publishDraft(cur.draftId)
      // 发布后草稿定格为 PUBLISHED:退出编辑器,避免僵尸态(后续保存/发布
      // 只会撞 409,被误读成校验未过)。
      setWs(null)
      setFlash(`已发布 ${detail.name} v${detail.version ?? ''}`)
      setIssues([])
      void refresh()
    } catch (err) {
      if (guard(err)) return
      if (err instanceof ApiError) {
        setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: err.code }])
      } else {
        setIssues([{ code: 'network_error', detail: '网络异常，发布未完成。' }])
      }
      setFlash('')
    }
  }, [guard, refresh])

  const value = useMemo<WorkflowValue>(
    () => ({ drafts, loading, ws, issues, flash, setFlash, editorKey, refresh, open, newLocal, close, onDocChange, save, publish }),
    [drafts, loading, ws, issues, flash, editorKey, refresh, open, newLocal, close, onDocChange, save, publish],
  )

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useWorkflow(): WorkflowValue {
  const v = useContext(Ctx)
  if (v === null) throw new Error('useWorkflow 必须在 WorkflowProvider 内使用')
  return v
}
