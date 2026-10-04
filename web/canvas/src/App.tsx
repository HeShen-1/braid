// App 根组件:登录 → 草稿列表 → 画布编辑 → 保存/发布(全走既有
// API 面,零新端点,D-t141)+ 执行视图(T16.6:触发/详情/回滚-续跑)。
// 401 统一登出回登录。
import { useCallback, useEffect, useState } from 'react'
import { ApiError, api, tokenStore, type DraftSummary, type Issue } from './api.ts'
import { LoginView } from './LoginView.tsx'
import { DraftList } from './DraftList.tsx'
import { CanvasEditor } from './CanvasEditor.tsx'
import { ExecutionsView } from './ExecutionsView.tsx'
import { blankDoc } from './defaults.ts'
import { serializeDoc } from './model/convert.ts'
import type { WorkflowDoc } from './model/dsl.ts'

interface Workspace {
  draftId: string | null
  doc: WorkflowDoc
}

export function App() {
  const [authed, setAuthed] = useState<boolean>(tokenStore.get() !== null)
  const [drafts, setDrafts] = useState<DraftSummary[]>([])
  const [ws, setWs] = useState<Workspace | null>(null)
  // 画布 remount 只发生在「打开草稿/新建」;首次保存(create 拿到 id)后
  // 保持挂载,不因 key 变化丢视口/选中态。
  const [editorKey, setEditorKey] = useState(0)
  const [issues, setIssues] = useState<Issue[]>([])
  const [flash, setFlash] = useState('')
  const [who, setWho] = useState('')
  const [view, setView] = useState<'drafts' | 'executions'>('drafts')

  const logout = useCallback(() => {
    tokenStore.clear()
    setAuthed(false)
    setWs(null)
    setDrafts([])
  }, [])

  const refreshDrafts = useCallback(async () => {
    try {
      const { items } = await api.listDrafts()
      setDrafts(items)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) logout()
    }
  }, [logout])

  useEffect(() => {
    if (authed) void refreshDrafts()
  }, [authed, refreshDrafts])

  const onLogin = useCallback(
    async (creds: { username: string; password: string }) => {
      const res = await api.login(creds.username, creds.password)
      tokenStore.set(res.token)
      setWho(creds.username)
      setAuthed(true)
    },
    [],
  )

  const openDraft = useCallback(
    async (id: string) => {
      try {
        const detail = await api.getDraft(id)
        setIssues([])
        setFlash('')
        setWs({ draftId: id, doc: detail.dsl })
        setEditorKey((k) => k + 1)
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          logout()
          return
        }
        setIssues([{ code: 'network_error', detail: '打开草稿失败,请稍后重试' }])
      }
    },
    [logout],
  )

  const newLocal = useCallback(() => {
    setIssues([])
    setFlash('')
    setWs({ draftId: null, doc: blankDoc() })
    setEditorKey((k) => k + 1)
  }, [])

  // 保存 = create-or-update:首次(本地新建)POST /definitions,之后 PUT。
  const save = useCallback(
    async (doc: WorkflowDoc) => {
      if (ws === null) return
      const serialized = serializeDoc(doc)
      try {
        if (ws.draftId === null) {
          const detail = await api.createDraft(serialized)
          setWs({ draftId: detail.id, doc })
          setFlash(`已创建草稿 #${detail.id}`)
        } else {
          await api.updateDraft(ws.draftId, serialized)
          setWs({ ...ws, doc })
          setFlash(`已保存草稿 #${ws.draftId}`)
        }
        setIssues([])
        void refreshDrafts()
      } catch (err) {
        if (err instanceof ApiError) {
          if (err.status === 401) {
            logout()
            return
          }
          setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: err.code }])
          setFlash('')
        } else {
          // 网络/解析异常不是校验问题:统一提示,避免停在过期 flash 上。
          setIssues([{ code: 'network_error', detail: '网络异常,保存未完成' }])
          setFlash('')
        }
      }
    },
    [ws, refreshDrafts, logout],
  )

  const publish = useCallback(async () => {
    if (ws?.draftId === null || ws === null) return
    try {
      const detail = await api.publishDraft(ws.draftId)
      // 发布成功草稿即定格为 PUBLISHED:回列表,避免编辑器僵尸态
      // (后续保存/发布只会撞 409 conflict,被误读成校验未过)。
      setWs(null)
      setFlash(`已发布 ${detail.name} v${detail.version ?? ''}`)
      setIssues([])
      void refreshDrafts()
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.status === 401) {
          logout()
          return
        }
        setIssues(err.issues.length > 0 ? err.issues : [{ code: err.code, detail: err.code }])
        setFlash('')
      } else {
        setIssues([{ code: 'network_error', detail: '网络异常,发布未完成' }])
        setFlash('')
      }
    }
  }, [ws, refreshDrafts, logout])

  if (!authed) {
    return (
      <div className="canvas-shell">
        <header className="canvas-header">
          <h1>braid 画布</h1>
        </header>
        <LoginView onLogin={onLogin} />
      </div>
    )
  }

  return (
    <div className="canvas-shell">
      <header className="canvas-header">
        <h1>braid 画布</h1>
        <span className="header-flash" role="status">
          {flash}
        </span>
        <div className="header-right">
          <nav className="view-nav" aria-label="视图切换">
            <button type="button" className={view === 'drafts' ? 'active' : ''} data-testid="nav-drafts" onClick={() => setView('drafts')}>
              草稿
            </button>
            <button type="button" className={view === 'executions' ? 'active' : ''} data-testid="nav-executions" onClick={() => setView('executions')}>
              执行
            </button>
          </nav>
          {who !== '' ? <span className="mono">{who}</span> : null}
          <button type="button" onClick={logout} data-testid="logout">
            退出
          </button>
        </div>
      </header>
      {/*
        草稿工作区恒挂载、仅切显隐(对抗审计 MEDIUM:条件卸载会把未保存的
        画布编辑态静默清空);执行视图轻状态,保持条件挂载。
      */}
      <div className={view === 'drafts' ? 'canvas-workspace' : 'canvas-workspace view-hidden'}>
        <DraftList drafts={drafts} activeId={ws?.draftId ?? null} onOpen={(id) => void openDraft(id)} onNew={newLocal} />
        {ws === null ? (
          <main className="canvas-main">
            <p>从左侧打开草稿,或新建工作流</p>
          </main>
        ) : (
          <div className="canvas-editor-host">
            <CanvasEditor
              key={editorKey}
              initialDoc={ws.doc}
              draftId={ws.draftId}
              onSave={save}
              onPublish={publish}
            />
            {issues.length > 0 ? <IssuesPanel issues={issues} /> : null}
          </div>
        )}
      </div>
      {view === 'executions' ? <ExecutionsView onAuthExpired={logout} /> : null}
    </div>
  )
}

function IssuesPanel({ issues }: { issues: Issue[] }) {
  return (
    <div className="issues" role="alert" data-testid="issues">
      <strong>操作未完成({issues.length} 项)</strong>
      <ul>
        {issues.map((iss, i) => (
          <li key={i}>
            <code>{iss.code}</code>
            {iss.node !== undefined && iss.node !== '' ? <> · 节点 <code>{iss.node}</code></> : null}
            {iss.ref !== undefined && iss.ref !== '' ? <> · <code>{iss.ref}</code></> : null}
            <span> — {iss.detail}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
