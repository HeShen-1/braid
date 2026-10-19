// 工作流面:左列表 + 右画布。URL 是地址(/workflows/:draftId),
// 文档真相在 WorkflowProvider(离开路由不丢编辑)。
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router'
import { Activity, FilePlus2, Rocket, Workflow } from 'lucide-react'
import { CanvasEditor } from '../CanvasEditor.tsx'
import { DraftList } from '../DraftList.tsx'
import { IssuesPanel } from '../ui/IssuesPanel.tsx'
import { EmptyState, Skeleton, Button } from '../ui/primitives.tsx'
import { useWorkflow } from '../workflow/WorkflowProvider.tsx'
import { I } from '../ui/I.tsx'
import { TID } from '../ui/testids.ts'
import { ConfirmLayer } from '../ui/confirm.tsx'
import type { WorkflowDoc } from '../model/dsl.ts'
import { useAuth } from '../auth.tsx'
import { PublishedVersions } from '../PublishedVersions.tsx'

export function WorkflowsRoute() {
  const { drafts, loading, listError, refresh, ws, dirty, issues, flash, editorKey, open, newLocal, onDocChange, save, publish } = useWorkflow()
  const params = useParams()
  const navigate = useNavigate()
  const [search] = useSearchParams()
  const { canAuthor, logout } = useAuth()
  const showVersions = !canAuthor || search.get('view') === 'versions'
  const urlId = params.draftId
  const workspaceRef = useRef(ws)
  workspaceRef.current = ws
  const [confirmNew, setConfirmNew] = useState(false)
  const newLayerRef = useRef<HTMLDivElement | null>(null)
  const newFocusRef = useRef<HTMLElement | null>(null)
  const wasConfirmNew = useRef(false)
  useEffect(() => {
    if (wasConfirmNew.current && !confirmNew && newFocusRef.current?.isConnected) newFocusRef.current.focus()
    wasConfirmNew.current = confirmNew
  }, [confirmNew])

  // URL → 状态:深链接与后退都走这一条。
  useEffect(() => {
    let active = true
    if (urlId !== undefined && !showVersions) void open(urlId).then((opened) => {
      if (!active || opened) return
      const id = workspaceRef.current?.draftId
      void navigate(id == null ? '/workflows' : `/workflows/${id}`, { replace: true })
    })
    return () => { active = false }
  }, [urlId, open, navigate, showVersions])

  // 状态 → URL:首次保存拿到 id 后把地址补上,刷新可回到同一张工作流。
  const previousId = useRef(ws?.draftId)
  useEffect(() => {
    const previous = previousId.current
    previousId.current = ws?.draftId
    if (previous === null && ws !== null && ws.draftId !== null && urlId !== ws.draftId) {
      void navigate(`/workflows/${ws.draftId}`, { replace: true })
    }
  }, [ws?.draftId, urlId, navigate])

  const createLocal = useCallback(() => {
    setConfirmNew(false)
    newLocal()
    void navigate('/workflows')
  }, [newLocal, navigate])
  const onNew = useCallback(() => {
    if (!dirty) { createLocal(); return }
    newFocusRef.current = document.activeElement as HTMLElement | null
    setConfirmNew(true)
  }, [dirty, createLocal])

  // 发布(F04):成功后草稿定格,编辑器随工作区卸载,退回列表地址。
  // 若在飞期间又有编辑,provider 保留工作区,这里也保留当前路由。
  const onPublish = useCallback(async (doc: WorkflowDoc) => {
    if (await publish(doc)) void navigate('/workflows', { replace: true })
  }, [publish, navigate])

  return (
    <div className="flex min-w-0 min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center gap-2 border-b border-line-0 px-4 py-2" aria-label="工作流视图">
        {canAuthor ? <Button size="sm" variant="quiet" aria-pressed={!showVersions} onClick={() => void navigate(ws?.draftId == null ? '/workflows' : `/workflows/${ws.draftId}`)}>草稿</Button> : null}
        <Button size="sm" variant="quiet" aria-pressed={showVersions} onClick={() => void navigate('/workflows?view=versions')}>已发布与版本历史</Button>
      </div>
      {showVersions ? <PublishedVersions canManage={canAuthor} onAuthExpired={logout} /> : <div className="flex min-w-0 min-h-0 flex-1">
      <DraftList
        drafts={drafts}
        activeId={ws?.draftId ?? null}
        onOpen={(id) => void navigate(`/workflows/${id}`)}
        onNew={onNew}
        loading={loading}
        error={listError}
        onRetry={() => void refresh()}
      />

      {ws === null ? (
        <div className="flex min-h-0 flex-1 flex-col">
          <IssuesPanel issues={issues} />
          {/* 列表态成功条(F04):ws 关闭后编辑器不在了,反馈不能跟着走。
              flash 只在发布成功路径上与 ws===null 同帧出现(打开/新建/关闭
              都会清 flash),所以这条等价于「发布成功条」。 */}
          {flash !== '' ? (
            <div className="wf-flash" role="status" data-testid={TID.workflowFlash}>
              <I glyph={Rocket} size={13} className="shrink-0" aria-hidden />
              <span className="min-w-0 truncate">{flash}</span>
              <Button
                size="sm"
                variant="quiet"
                icon={Activity}
                className="ml-auto shrink-0"
                data-testid={TID.viewPublished}
                onClick={() => void navigate('/workflows?view=versions')}
              >
                查看已发布
              </Button>
            </div>
          ) : null}
          <div className="flex min-h-0 flex-1 flex-col justify-center">
            {loading ? (
              <div className="flex flex-col items-center gap-3 p-6">
                <Skeleton width={220} height={16} />
                <Skeleton width={420} height={12} />
                <Skeleton width={360} height={12} />
              </div>
            ) : listError !== '' ? (
              <EmptyState icon={Workflow} title="工作流列表尚未读取成功" hint={listError}
                action={<Button onClick={() => void refresh()}>重新读取工作流列表</Button>} />
            ) : (
              <EmptyState
                icon={Workflow}
                title={drafts.length === 0 ? '新建你的第一条工作流' : '从左侧打开一条工作流'}
                hint="画布上编排节点与连线，服务端按 JSON DSL 校验；发布后即可在执行页触发。"
                action={
                  <Button icon={FilePlus2} onClick={onNew}>
                    新建工作流
                  </Button>
                }
              />
            )}
          </div>
        </div>
      ) : (
        <div className="flex min-h-0 flex-1 flex-col">
          {confirmNew ? (
            <ConfirmLayer
              layerRef={newLayerRef}
              label="新建前确认"
              hint="当前画布尚未保存，新建会丢弃这些更改。"
              confirmText="丢弃并新建"
              onConfirm={createLocal}
              onDismiss={() => setConfirmNew(false)}
            />
          ) : null}
          <CanvasEditor
            key={editorKey}
            initialDoc={ws.doc}
            draftId={ws.draftId}
            dirty={dirty}
            flash={flash}
            onDocChange={onDocChange}
            onSave={save}
            onPublish={onPublish}
          />
          <IssuesPanel issues={issues} />
        </div>
      )}
      </div>}
    </div>
  )
}
