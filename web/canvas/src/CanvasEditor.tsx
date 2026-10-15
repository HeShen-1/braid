// 画布编辑器:DSL → React Flow 双向接线。
// 结构 = 顶栏(身份 / flash 状态文案 / 动作)+ 三栏(工具条 · 画布 · 属性
// 面板)+ 底部 DSL 预览抽屉;画布、面板与预览读的是同一份派生文档。
// 状态面:base(文档级字段,工作流面板可改)+ RF 节点/边(spec 引用即
// 文档节点,透传契约见 model/convert)+ viewport(onMoveEnd 采集)。
// 导出 = fromCanvas 随时可算(纯派生);每次变更经 onDocChange 上报壳层,
// 切路由不丢未保存的编辑。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type NodeTypes,
  type Viewport,
} from '@xyflow/react'
import { ChevronRight, Info, MousePointerClick, PanelRightClose, PanelRightOpen, Rocket, Save, Settings2, Workflow } from 'lucide-react'
import { clampViewport, connectEdges, fromCanvas, nextNodeId, reconcileSourceHandles, serializeDoc, toCanvas } from './model/convert.ts'
import type { FlowNode, FlowEdge } from './model/convert.ts'
import type { DslNode, NodeType, WorkflowDoc } from './model/dsl.ts'
import { nodeTypes } from './nodes/NodeViews.tsx'
import { Palette } from './Palette.tsx'
import { NodePanel } from './NodePanel.tsx'
import { WorkflowPanel } from './WorkflowPanel.tsx'
import { defaultSpec } from './defaults.ts'
import { PANEL_RIGHT_KEY, useCollapsed } from './prefs.ts'
import { Button, EmptyState } from './ui/primitives.tsx'
import { ConfirmLayer } from './ui/confirm.tsx'
import { I } from './ui/I.tsx'
import { TID } from './ui/testids.ts'

const flowNodeTypes = nodeTypes as unknown as NodeTypes

function Editor({
  initialDoc,
  draftId,
  dirty,
  flash,
  onDocChange,
  onSave,
  onPublish,
}: {
  initialDoc: WorkflowDoc
  draftId: string | null
  dirty?: boolean
  flash?: string
  onDocChange?: (doc: WorkflowDoc) => void
  onSave?: (doc: WorkflowDoc) => void | Promise<void>
  onPublish?: (doc: WorkflowDoc) => void | Promise<void>
}) {
  const [base, setBase] = useState(initialDoc)
  const initial = useMemo(() => toCanvas(initialDoc), [initialDoc])
  // 文档存过视口 → 应用它(只写不读会让保存值与所见脱节);未存过
  // (无 canvas meta 的既有草稿)→ fitView 首开自适应。zoom 边界对齐
  // schema 词域 0.1..4(RF 默认 0.5..2 会把合法 0.2 文档顶到 0.5 显示)。
  const hadStoredViewport = initialDoc.metadata?.canvas?.viewport !== undefined
  const [nodes, setNodes, onNodesChange] = useNodesState<FlowNode>(initial.nodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState<FlowEdge>(initial.edges)
  const [viewport, setViewport] = useState(initial.viewport)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [showWorkflowPanel, setShowWorkflowPanel] = useState(false)
  // 右栏(属性面板)折叠态:跨会话 UI 偏好(prefs.ts),与左栏同一口径。
  const [sideCollapsed, setSideCollapsed] = useCollapsed(PANEL_RIGHT_KEY)
  const { screenToFlowPosition, deleteElements } = useReactFlow()

  // 节点删除(T21.4 ④):危险动作只留一条漏斗 —— 面板「删除节点」入口与
  // 键盘 Delete/Backspace 都先落到 onBeforeDelete,布防确认层;确认后才真删。
  // 纯边删除(可重连、无级联)不拦,保持画布直接操作的快路径。
  const [pendingDelete, setPendingDelete] = useState<{ nodes: FlowNode[]; edges: FlowEdge[] } | null>(null)
  // 确认后放行同一次删除:否则 deleteElements 再次落到 onBeforeDelete 上自我拦截。
  const allowDelete = useRef(false)
  const deleteLayerRef = useRef<HTMLDivElement | null>(null)
  const deleteArmRef = useRef<HTMLButtonElement | null>(null)
  // 焦点回归(T21.4 增项):收起确认层后回到触发点 —— 面板开着就回入口按钮,
  // 面板收起(键盘触发)就回节点卡片,不把焦点丢回 body。
  const lastPending = useRef<{ nodes: FlowNode[] } | null>(null)

  const onBeforeDelete = useCallback(async ({ nodes: delNodes, edges: delEdges }: { nodes: FlowNode[]; edges: FlowEdge[] }) => {
    if (allowDelete.current) {
      allowDelete.current = false
      return true
    }
    if (delNodes.length === 0) return true
    setPendingDelete({ nodes: delNodes, edges: delEdges })
    return false
  }, [])

  const confirmDelete = useCallback(async () => {
    const pending = pendingDelete
    setPendingDelete(null)
    if (pending === null) return
    allowDelete.current = true
    await deleteElements(pending)
  }, [deleteElements, pendingDelete])

  useEffect(() => {
    if (pendingDelete !== null) {
      lastPending.current = pendingDelete
      return
    }
    const prev = lastPending.current
    if (prev === null) return
    lastPending.current = null
    const arm = deleteArmRef.current
    if (arm !== null) {
      arm.focus()
      return
    }
    const id = prev.nodes[0]?.id
    if (id !== undefined) document.querySelector<HTMLElement>(`.react-flow__node[data-id="${CSS.escape(id)}"]`)?.focus()
  }, [pendingDelete])

  // 面板入口只是把节点交给同一条漏斗:不自己删,避免两条删除路径分叉。
  const requestDelete = useCallback(
    (id: string) => {
      void deleteElements({ nodes: [{ id }] })
    },
    [deleteElements],
  )

  const onConnect = useCallback(
    (c: Connection) => {
      // 同一对节点只留一条边(重复边撞 T1.2 的 duplicate_edge);再从别的锚点
      // 连一次 = 改绑该边的源锚点(T21.4 ③)。判定是纯函数,见 connectEdges。
      setEdges((eds) => connectEdges(eds, c))
    },
    [setEdges],
  )
  const onMoveEnd = useCallback((_: unknown, vp: Viewport) => setViewport(clampViewport(vp)), [])
  const onSelectionChange = useCallback(({ nodes: sel }: { nodes: FlowNode[] }) => {
    setShowWorkflowPanel(false)
    setSelectedId(sel.length === 1 ? sel[0].id : null)
  }, [])

  const addNode = useCallback(
    (type: NodeType) => {
      // 视口中心落点;palette 基名 llm/tool/branch/approval/fan 均合规。
      const position = screenToFlowPosition({ x: window.innerWidth / 2, y: window.innerHeight / 2 })
      const id = nextNodeId(type === 'parallel' ? 'fan' : type, nodes.map((n) => n.id))
      setNodes((nds) => [...nds, { id, type, position, data: { spec: defaultSpec(type, id) } }])
    },
    [nodes, screenToFlowPosition, setNodes],
  )

  // immutable patch:换 spec/data 引用触发 RF 节点重渲(直接改字段会被
  // 节点组件 memo 吞掉)。
  const updateSpec = useCallback(
    (id: string, patch: (s: DslNode) => DslNode) => {
      setNodes((nds) => nds.map((n) => (n.id === id ? { ...n, data: { spec: patch(n.data.spec) } } : n)))
    },
    [setNodes],
  )

  // 锚点对账(spec 变 → 锚点集合变):清掉绑在已消失锚点上的边(T21.4 ③),
  // 否则它们会被 React Flow 静默丢掉(按 id 找不到手柄就不画)。
  useEffect(() => {
    setEdges((eds) => reconcileSourceHandles(nodes, eds) ?? eds)
  }, [nodes, setEdges])

  const doc = useMemo(() => fromCanvas({ nodes, edges, viewport }, base), [nodes, edges, viewport, base])
  const selected = nodes.find((n) => n.id === selectedId)
  // 确认条文案:把「要删什么、连带什么」说清(危险动作的确认必须可核对)。
  const deleteHint =
    pendingDelete === null
      ? ''
      : `${pendingDelete.nodes.length === 1 ? `删除节点 ${pendingDelete.nodes[0].id}` : `删除 ${pendingDelete.nodes.length} 个节点`}${
          pendingDelete.edges.length === 0 ? '' : `会同时移除 ${pendingDelete.edges.length} 条连线`
        },不可撤销。`

  // 文档变更上报:挂载那一次不算变更(壳层打开草稿时已持有同一份文档),
  // 之后每次编辑都上报 —— 壳层据此在切路由/切执行面时保住未保存的编辑。
  const reportedRef = useRef(doc)
  const onDocChangeRef = useRef(onDocChange)
  onDocChangeRef.current = onDocChange
  useEffect(() => {
    if (reportedRef.current === doc) return
    reportedRef.current = doc
    onDocChangeRef.current?.(doc)
  }, [doc])

  const [busy, setBusy] = useState(false)
  const run = useCallback(
    (fn: () => Promise<void> | void) => async () => {
      setBusy(true)
      try {
        await fn()
      } finally {
        setBusy(false)
      }
    },
    [],
  )

  const toggleWorkflow = useCallback(() => {
    // 面板收起时点「工作流设置」:先把面板放回来,否则点了像没反应。
    setSideCollapsed(false)
    setShowWorkflowPanel((v) => !v)
    setSelectedId(null)
  }, [setSideCollapsed])

  return (
    <div className="editor" data-testid={TID.editor}>
      <header className="editor-bar">
        <I glyph={Workflow} size={13} className="shrink-0 text-ink-2" />
        <span className="mono min-w-0 truncate text-xs text-ink-0" title={base.name}>
          {base.name}
        </span>
        <span className="num shrink-0 text-micro text-ink-3">{draftId === null ? '未建档' : `#${draftId}`}</span>
        {dirty ? <span className="shrink-0 text-micro text-ink-2">未保存</span> : null}
        {flash !== undefined && flash !== '' ? (
          <span
            className="flex min-w-0 items-center gap-1.5 border-l border-line-0 pl-3 text-xs text-ink-1"
            role="status"
            data-testid={TID.editorFlash}
          >
            <I glyph={Info} size={12} className="shrink-0 text-ink-2" />
            <span className="truncate">{flash}</span>
          </span>
        ) : null}
        <div className="ml-auto flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant="quiet"
            iconOnly
            icon={sideCollapsed ? PanelRightOpen : PanelRightClose}
            aria-label={sideCollapsed ? '展开属性面板' : '收起属性面板'}
            aria-pressed={!sideCollapsed}
            data-testid={TID.panelRightToggle}
            onClick={() => setSideCollapsed(!sideCollapsed)}
          />
          <Button
            size="sm"
            variant={showWorkflowPanel ? 'default' : 'quiet'}
            icon={Settings2}
            aria-pressed={showWorkflowPanel}
            onClick={toggleWorkflow}
          >
            工作流设置
          </Button>
          {onSave !== undefined ? (
            <Button size="sm" variant="primary" icon={Save} data-testid={TID.editorSave} disabled={busy} onClick={run(() => onSave(doc))}>
              保存草稿
            </Button>
          ) : null}
          {onPublish !== undefined && draftId !== null ? (
            <Button size="sm" icon={Rocket} data-testid={TID.editorPublish} disabled={busy} onClick={run(() => onPublish(doc))}>
              发布
            </Button>
          ) : null}
        </div>
      </header>

      <div className="editor-mid">
        <Palette onAdd={addNode} />

        <div className="editor-canvas">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={flowNodeTypes}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onMoveEnd={onMoveEnd}
            onSelectionChange={onSelectionChange}
            onBeforeDelete={onBeforeDelete}
            deleteKeyCode={['Backspace', 'Delete']}
            minZoom={0.1}
            maxZoom={4}
            defaultViewport={hadStoredViewport ? initial.viewport : undefined}
            fitView={!hadStoredViewport}
          >
            {/* 地面 = 近黑 + 发丝线网格(未点亮的荧光屏),不铺点阵噪点。 */}
            <Background variant={BackgroundVariant.Lines} gap={28} lineWidth={1} color="var(--color-line-0)" />
            <Controls />
            <MiniMap pannable zoomable nodeColor="var(--color-line-2)" ariaLabel="画布缩略图" />
          </ReactFlow>
          {nodes.length === 0 ? (
            <div className="pointer-events-none absolute inset-0 grid place-items-center">
              <EmptyState
                icon={MousePointerClick}
                title="空白画布"
                hint="用左侧工具条添加第一个节点，再拖动节点边缘的圆点连线。"
              />
            </div>
          ) : null}
          {/* 删除确认层落在画布下缘:入口在右栏、键盘在画布上,两处触发都看得见同一个条。 */}
          {pendingDelete !== null ? (
            <ConfirmLayer
              className="canvas-confirm"
              layerRef={deleteLayerRef}
              label="确认删除"
              hint={deleteHint}
              confirmText="确认删除"
              testId={TID.deleteConfirm}
              confirmTestId={TID.deleteConfirmSubmit}
              onConfirm={() => void confirmDelete()}
              onDismiss={() => setPendingDelete(null)}
            />
          ) : null}
        </div>

        {/* 属性面板常驻:画布宽度不随选中态跳动。收起时整列让给画布,
            开关留在顶栏(收起后仍可达,不靠悬浮按钮)。 */}
        {sideCollapsed ? null : (
          <aside className="editor-side" aria-label="属性面板">
            {selected !== undefined ? (
              /* key = 节点 id:切换节点必须重建面板本地态(如 ApproversInput
                 的 raw 暂存),否则残留上一节点的输入并覆盖下一节点(对抗审计 HIGH)。 */
              <NodePanel
                key={selected.id}
                spec={selected.data.spec}
                update={(patch) => updateSpec(selected.id, patch)}
                onDelete={() => requestDelete(selected.id)}
                deleteArmRef={deleteArmRef}
              />
            ) : showWorkflowPanel ? (
              <WorkflowPanel
                key={draftId ?? 'local'}
                doc={base}
                update={(patch) => setBase((b) => patch(b))}
                nameLocked={draftId !== null || busy}
              />
            ) : (
              <EmptyState
                icon={MousePointerClick}
                title="未选中节点"
                hint="在画布上点选一个节点编辑它的属性；顶栏「工作流设置」切换文档级字段。"
              />
            )}
          </aside>
        )}
      </div>

      {/* DSL 预览抽屉:导出产物,只读;折叠时只是一条横档。 */}
      <details className="editor-json">
        <summary>
          <I glyph={ChevronRight} size={12} className="editor-json-caret" />
          <span className="micro">DSL 预览</span>
          <span className="text-micro text-ink-3">导出产物 · 只读</span>
        </summary>
        <pre data-testid={TID.editorJson}>{serializeDoc(doc)}</pre>
      </details>
    </div>
  )
}

export function CanvasEditor(props: {
  initialDoc: WorkflowDoc
  draftId?: string | null
  dirty?: boolean
  /** 顶栏状态文案(保存/发布的结果由壳层给)。 */
  flash?: string
  /** 每次文档变更上报(挂载不算),供壳层保态。 */
  onDocChange?: (doc: WorkflowDoc) => void
  onSave?: (doc: WorkflowDoc) => void | Promise<void>
  onPublish?: (doc: WorkflowDoc) => void | Promise<void>
}) {
  return (
    <ReactFlowProvider>
      <Editor
        initialDoc={props.initialDoc}
        draftId={props.draftId ?? null}
        dirty={props.dirty}
        flash={props.flash}
        onDocChange={props.onDocChange}
        onSave={props.onSave}
        onPublish={props.onPublish}
      />
    </ReactFlowProvider>
  )
}
