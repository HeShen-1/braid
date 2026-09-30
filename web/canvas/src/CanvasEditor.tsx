// 画布编辑器:DSL → React Flow 双向接线。
// 状态面:base(文档级字段,工作流面板可改)+ RF 节点/边(spec 引用即
// 文档节点,透传契约见 model/convert)+ viewport(onMoveEnd 采集)。
// 导出 = fromCanvas 随时可算(纯派生),任务 6 接 PUT/publish。
import { useCallback, useMemo, useState } from 'react'
import {
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type NodeTypes,
  type Viewport,
} from '@xyflow/react'
import { clampViewport, fromCanvas, nextNodeId, serializeDoc, toCanvas } from './model/convert.ts'
import type { FlowNode, FlowEdge } from './model/convert.ts'
import type { DslNode, NodeType, WorkflowDoc } from './model/dsl.ts'
import { nodeTypes } from './nodes/NodeViews.tsx'
import { Palette } from './Palette.tsx'
import { NodePanel } from './NodePanel.tsx'
import { WorkflowPanel } from './WorkflowPanel.tsx'
import { defaultSpec } from './defaults.ts'

const flowNodeTypes = nodeTypes as unknown as NodeTypes

function Editor({
  initialDoc,
  draftId,
  onSave,
  onPublish,
}: {
  initialDoc: WorkflowDoc
  draftId: string | null
  onSave?: (doc: WorkflowDoc) => void | Promise<void>
  onPublish?: () => void | Promise<void>
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
  const { screenToFlowPosition } = useReactFlow()

  const onConnect = useCallback((c: Connection) => {
    setEdges((eds) => addEdge(c, eds))
  }, [setEdges])
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

  const doc = useMemo(() => fromCanvas({ nodes, edges, viewport }, base), [nodes, edges, viewport, base])
  const selected = nodes.find((n) => n.id === selectedId)
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

  return (
    <div className="editor">
      <div className="editor-bar">
        <Palette onAdd={addNode} />
        <button
          type="button"
          className="palette-btn"
          data-testid="toggle-workflow"
          onClick={() => {
            setShowWorkflowPanel((v) => !v)
            setSelectedId(null)
          }}
        >
          工作流设置
        </button>
        <span className="bar-spacer" />
        {onSave !== undefined ? (
          <button type="button" className="palette-btn primary" data-testid="save-draft" disabled={busy} onClick={run(() => onSave(doc))}>
            保存草稿
          </button>
        ) : null}
        {onPublish !== undefined && draftId !== null ? (
          <button type="button" className="palette-btn" data-testid="publish" disabled={busy} onClick={run(onPublish)}>
            发布
          </button>
        ) : null}
      </div>
      <div className="editor-mid">
        <div className="editor-canvas" data-testid="canvas">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={flowNodeTypes}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onMoveEnd={onMoveEnd}
            onSelectionChange={onSelectionChange}
            deleteKeyCode={['Backspace', 'Delete']}
            minZoom={0.1}
            maxZoom={4}
            defaultViewport={hadStoredViewport ? initial.viewport : undefined}
            fitView={!hadStoredViewport}
          >
            <Background />
            <Controls />
            <MiniMap pannable zoomable />
          </ReactFlow>
        </div>
        {selected ? (
          <aside className="editor-side">
            {/* key = 节点 id:切换节点必须重建面板本地态(如 ApproversInput
                的 raw 暂存),否则残留上一节点的输入并覆盖下一节点(对抗审计 HIGH)。 */}
            <NodePanel key={selected.id} spec={selected.data.spec} update={(patch) => updateSpec(selected.id, patch)} />
          </aside>
        ) : showWorkflowPanel ? (
          <aside className="editor-side">
            <WorkflowPanel
              key={draftId ?? 'local'}
              doc={base}
              update={(patch) => setBase((b) => patch(b))}
              nameLocked={draftId !== null}
            />
          </aside>
        ) : null}
      </div>
      <details className="editor-json">
        <summary>DSL 预览(导出产物)</summary>
        <pre aria-label="dsl-preview">{serializeDoc(doc)}</pre>
      </details>
    </div>
  )
}

export function CanvasEditor(props: {
  initialDoc: WorkflowDoc
  draftId?: string | null
  onSave?: (doc: WorkflowDoc) => void | Promise<void>
  onPublish?: () => void | Promise<void>
}) {
  return (
    <ReactFlowProvider>
      <Editor initialDoc={props.initialDoc} draftId={props.draftId ?? null} onSave={props.onSave} onPublish={props.onPublish} />
    </ReactFlowProvider>
  )
}
