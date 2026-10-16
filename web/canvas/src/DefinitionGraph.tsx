// 定义版本图(T21.1 Lane B 增量):执行详情里的「当时声明的形状」。
//
// 时间带只表达时间序;DAG 拓扑(谁喂谁、分支怎么裁)只能从图读 —— 审计与
// 排障要的是"这次执行当初到底声明了什么形状"。只读概览:节点不可拖/不可连、
// fitView 适配、滚轮透传页面(不抢页面滚动)。
//
// 按需加载:默认收起 = 零请求;展开才拉 GET /definitions/{name}/versions/{version}
// (端点既有,api.ts 不改)。失败给就近错误条并带重试,不阻塞详情其余部分。
//
// 与 Lane A 的注入契约:`data:{spec, status?}` —— status 是**当前执行该节点的
// 聚合状态**(多实例 → 单节点一色,失败优先),所以图上直接看出哪一步成功/失败/挂起。
// 刻意不注入 instanceNo:NodeShell 在 instanceNo 在场时会渲染
// tid.nodeRow(id, inst) 测试锚,与时间带的行锚重名(e2e/测试立刻变 multiple);
// 实例级细节本来也是时间带的职责。未传 instanceNo ⇒ 图上用 tid.nodeCard(id)。
import { useEffect, useId, useMemo, useState } from 'react'
import { ReactFlow } from '@xyflow/react'
import { ChevronDown, ChevronUp, TriangleAlert } from 'lucide-react'
import { ApiError, api } from './api.ts'
import { toCanvas } from './model/convert.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { nodeTypes } from './nodes/NodeViews.tsx'
import { Alert, Button, PanelHead, Skeleton } from './ui/primitives.tsx'
import { apiErrorText, errorCodeText } from './ui/errors.ts'

export function DefinitionGraph({
  name,
  version,
  statuses,
  onAuthExpired,
}: {
  name: string
  version: number
  /** 节点 id → 当前执行的聚合状态;缺省 = 该节点这次没有执行行(图上不显示状态)。 */
  statuses: Record<string, string>
  onAuthExpired: () => void
}) {
  const [open, setOpen] = useState(false)
  const [doc, setDoc] = useState<WorkflowDoc | null>(null)
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)
  const bodyId = useId()

  useEffect(() => {
    if (!open || doc !== null) return
    let cancelled = false
    api
      .getVersion(name, version)
      .then((d) => {
        if (cancelled) return
        // 契约上 dsl 必在场;读回缺字段时按失败处理,不装死骨架。
        const loaded = d.dsl as WorkflowDoc | null | undefined
        if (loaded === null || loaded === undefined) {
          setError(errorCodeText('definition_empty'))
          return
        }
        setDoc(loaded)
        setError('')
      })
      .catch((err: unknown) => {
        if (cancelled) return
        if (err instanceof ApiError && err.status === 401) {
          onAuthExpired()
          return
        }
        setError(apiErrorText(err))
      })
    return () => {
      cancelled = true
    }
  }, [open, doc, attempt, name, version, onAuthExpired])

  const flow = useMemo(() => {
    if (doc === null) return null
    try {
      return toCanvas(doc)
    } catch {
      return null // 定义内容异常:给就地提示,不让详情整面炸掉
    }
  }, [doc])

  // 只读:位置来自定义版本(或自动布局),交互位全部关掉。
  const flowNodes = (flow?.nodes ?? []).map((n) => ({
    id: n.id,
    type: n.type,
    position: n.position,
    data: { spec: n.data.spec, status: statuses[n.id] },
    draggable: false,
    connectable: false,
    selectable: false,
    focusable: false,
  }))
  const flowEdges = (flow?.edges ?? []).map((e) => ({ ...e, selectable: false, focusable: false }))

  const counts = flow !== null ? ` · ${flow.nodes.length} 节点 · ${flow.edges.length} 边` : ''

  return (
    <section className="panel exec-graph" aria-label="定义版本图">
      <PanelHead
        title="定义版本图"
        sub={`${name} v${version}${counts}`}
        actions={
          <Button
            variant="quiet"
            size="sm"
            icon={open ? ChevronUp : ChevronDown}
            aria-expanded={open}
            aria-controls={bodyId}
            aria-label={open ? '收起定义版本图' : '展开定义版本图'}
            onClick={() => setOpen(!open)}
          >
            {open ? '收起' : '展开'}
          </Button>
        }
      />
      {/* 收起时内容不挂载(零请求、零测量),容器保留 → aria-controls 恒可解析 */}
      <div id={bodyId} className="exec-graph-body" hidden={!open}>
        {open ? (
          <>
            {error !== '' ? (
              <Alert
                tone="error"
                icon={TriangleAlert}
                actions={
                  <Button
                    size="sm"
                    onClick={() => {
                      setError('')
                      setAttempt((a) => a + 1)
                    }}
                  >
                    重试
                  </Button>
                }
              >
                定义版本图加载失败：{error}
              </Alert>
            ) : null}
            {doc === null && error === '' ? <Skeleton height={200} /> : null}
            {flow !== null ? (
              <div className="exec-graph-canvas">
                <ReactFlow
                  nodes={flowNodes}
                  edges={flowEdges}
                  nodeTypes={nodeTypes}
                  fitView
                  fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
                  nodesDraggable={false}
                  nodesConnectable={false}
                  elementsSelectable={false}
                  nodesFocusable={false}
                  edgesFocusable={false}
                  zoomOnScroll={false}
                  zoomOnDoubleClick={false}
                  panOnScroll={false}
                  preventScrolling={false}
                  minZoom={0.2}
                  maxZoom={1}
                />
              </div>
            ) : null}
            {doc !== null && flow === null ? (
              <p className="field-error">定义内容无法绘制(节点数据异常),其余详情不受影响。</p>
            ) : null}
          </>
        ) : null}
      </div>
    </section>
  )
}
