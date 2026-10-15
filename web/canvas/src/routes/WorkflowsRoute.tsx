// 工作流面:左列表 + 右画布。URL 是地址(/workflows/:draftId),
// 文档真相在 WorkflowProvider(离开路由不丢编辑)。
import { useCallback, useEffect } from 'react'
import { useNavigate, useParams } from 'react-router'
import { Activity, FilePlus2, Rocket, Workflow } from 'lucide-react'
import { CanvasEditor } from '../CanvasEditor.tsx'
import { DraftList } from '../DraftList.tsx'
import { IssuesPanel } from '../ui/IssuesPanel.tsx'
import { EmptyState, Skeleton, Button } from '../ui/primitives.tsx'
import { useWorkflow } from '../workflow/WorkflowProvider.tsx'
import { I } from '../ui/I.tsx'
import { TID } from '../ui/testids.ts'

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

  // 发布(F04):成功后草稿定格,编辑器随工作区卸载;深链若继续指着它,
  // 上面的 URL→状态 effect 会回弹 open —— open() 对非 DRAFT 的拒绝兜底
  // (不复活编辑器、不清 flash),这里退回列表地址是 URL 卫生。
  const onPublish = useCallback(async () => {
    if (await publish()) void navigate('/workflows', { replace: true })
  }, [publish, navigate])

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
                onClick={() => void navigate('/executions')}
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
            ) : (
              <EmptyState
                icon={Workflow}
                title={drafts.length === 0 ? '新建你的第一条工作流' : '从左侧打开一条工作流'}
                hint="画布上编排节点与连线，服务端按 JSON DSL 校验；发布后即可在执行面触发。"
                action={
                  <Button icon={FilePlus2} onClick={newLocal}>
                    新建工作流
                  </Button>
                }
              />
            )}
          </div>
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
            onPublish={onPublish}
          />
          <IssuesPanel issues={issues} />
        </div>
      )}
    </div>
  )
}
