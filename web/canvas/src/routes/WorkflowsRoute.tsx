// 工作流面:左列表 + 右画布。URL 是地址(/workflows/:draftId),
// 文档真相在 WorkflowProvider(离开路由不丢编辑)。
import { useEffect } from 'react'
import { useNavigate, useParams } from 'react-router'
import { CanvasEditor } from '../CanvasEditor.tsx'
import { DraftList } from '../DraftList.tsx'
import { IssuesPanel } from '../ui/IssuesPanel.tsx'
import { EmptyState, Skeleton } from '../ui/primitives.tsx'
import { Workflow } from 'lucide-react'
import { useWorkflow } from '../workflow/WorkflowProvider.tsx'

export function WorkflowsRoute() {
  const { drafts, loading, ws, issues, flash, editorKey, open, newLocal, onDocChange, save, publish } = useWorkflow()
  const params = useParams()
  const navigate = useNavigate()
  const urlId = params.draftId

  // URL → 状态:深链接与后退都走这一条。
  useEffect(() => {
    if (urlId !== undefined && ws?.draftId !== urlId) void open(urlId)
  }, [urlId, ws?.draftId, open])

  // 状态 → URL:首次保存拿到 id 后把地址补上,刷新可回到同一张工作流。
  useEffect(() => {
    if (ws !== null && ws.draftId !== null && urlId !== ws.draftId) {
      void navigate(`/workflows/${ws.draftId}`, { replace: true })
    }
  }, [ws?.draftId, urlId, navigate])

  return (
    <div className="flex min-h-0 flex-1">
      <DraftList
        drafts={drafts}
        activeId={ws?.draftId ?? null}
        onOpen={(id) => void navigate(`/workflows/${id}`)}
        onNew={() => {
          void navigate('/workflows')
          newLocal()
        }}
      />

      {ws === null ? (
        <div className="flex min-h-0 flex-1 flex-col">
          {loading ? (
            <div className="flex flex-col gap-3 p-6">
              <Skeleton width={220} height={16} />
              <Skeleton width={420} height={12} />
              <Skeleton width={360} height={12} />
            </div>
          ) : (
            <EmptyState
              icon={Workflow}
              title={drafts.length === 0 ? '新建你的第一条工作流' : '从左侧打开一条工作流'}
              hint="画布上编排节点与连线，服务端按 JSON DSL 校验；发布后即可在执行面触发。"
            />
          )}
        </div>
      ) : (
        <div className="flex min-h-0 flex-1 flex-col">
          <CanvasEditor
            key={editorKey}
            initialDoc={ws.doc}
            draftId={ws.draftId}
            flash={flash}
            onDocChange={onDocChange}
            onSave={save}
            onPublish={publish}
          />
          <IssuesPanel issues={issues} />
        </div>
      )}
    </div>
  )
}
