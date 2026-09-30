// DSL ⇄ 画布双向转换(纯函数,零 React Flow 依赖——产出 RF 形状的普通
// 对象,组件层直接喂 <ReactFlow>)。
// 导出纪律:
//   - 服务端 MySQL JSON 列读回会规范化键序(D-t3.6),导入原文顺序不可
//     依赖 → serializeDoc 走 canonical 键序,任何来源的文档导出字节稳定;
//   - 画布不改的字段(nodes 专有字段/inputs/outputs/description)原样
//     透传(引用不复制),面板编辑 spec 对象即编辑未来导出内容;
//   - metadata.canvas 画布全量重写(位置取整、zoom 夹 0.1..4),被删
//     节点的陈旧位置条目随之消失。

import type {
  ApprovalNode,
  BranchCase,
  BranchNode,
  CanvasMeta,
  DslEdge,
  DslNode,
  LlmNode,
  ParallelBody,
  Point,
  ToolNode,
  Viewport,
  WorkflowDoc,
} from './dsl.ts'

export interface FlowNode {
  id: string
  type: DslNode['type']
  position: Point
  data: { spec: DslNode }
}

export interface FlowEdge {
  id: string
  source: string
  target: string
  label?: string
}

export interface CanvasState {
  nodes: FlowNode[]
  edges: FlowEdge[]
  viewport: Viewport
}

const ZOOM_MIN = 0.1
const ZOOM_MAX = 4
const COL = 260
const ROW = 140
const ORIGIN = 40

export function clampViewport(v?: Partial<Viewport>): Viewport {
  const zoom = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, v?.zoom ?? 1))
  return { x: roundCoord(v?.x), y: roundCoord(v?.y), zoom }
}

// Math.round(-0.2) 产出 -0,Object.is 下 -0 ≠ 0,序列化也是 "-0"——
// 归一到 +0 保 canonical 字节与断言稳定;非有限数(NaN/Inf,防御面:
// 生产路径不可达但 JSON.stringify 会把 NaN 编码成 null 违 schema)归零。
function roundCoord(v: number | undefined): number {
  const n = v ?? 0
  if (!Number.isFinite(n)) return 0
  const r = Math.round(n)
  return r === 0 ? 0 : r
}

/**
 * autoLayout:按最长路径分层(入度 0 = 第 0 层),层内按文档顺序排。
 * 任一节点缺位置即整体重排(混用已存位置与猜测位置必产生重叠,不如
 * 全新布局可预期)。环(防御:schema 外先到)按已访问即定层处理。
 */
export function autoLayout(doc: WorkflowDoc): Map<string, Point> {
  const incoming = new Map<string, string[]>()
  for (const n of doc.nodes) incoming.set(n.id, [])
  for (const e of doc.edges ?? []) incoming.get(e.to)?.push(e.from)
  const depth = new Map<string, number>()
  const visiting = new Set<string>()
  const compute = (id: string): number => {
    const known = depth.get(id)
    if (known !== undefined) return known
    if (visiting.has(id)) return 0 // 环防御:按当前层处理,不递归
    visiting.add(id)
    const parents = incoming.get(id) ?? []
    const d = parents.length === 0 ? 0 : Math.max(...parents.map(compute)) + 1
    visiting.delete(id)
    depth.set(id, d)
    return d
  }
  for (const n of doc.nodes) compute(n.id)
  const byLayer = new Map<number, string[]>()
  for (const n of doc.nodes) {
    const d = depth.get(n.id) ?? 0
    const slot = byLayer.get(d) ?? []
    slot.push(n.id)
    byLayer.set(d, slot)
  }
  const out = new Map<string, Point>()
  for (const [d, ids] of byLayer) {
    ids.forEach((id, i) => {
      out.set(id, { x: ORIGIN + d * COL, y: ORIGIN + i * ROW })
    })
  }
  return out
}

/** DSL → 画布:位置取 metadata.canvas,缺任一即整体 autoLayout。 */
export function toCanvas(doc: WorkflowDoc): CanvasState {
  const metaNodes = doc.metadata?.canvas?.nodes
  const complete = metaNodes !== undefined && doc.nodes.every((n) => metaNodes[n.id]?.position)
  const layout = complete ? null : autoLayout(doc)
  const nodes: FlowNode[] = doc.nodes.map((n) => ({
    id: n.id,
    type: n.type,
    position: complete ? { ...metaNodes![n.id].position } : layout!.get(n.id)!,
    data: { spec: n },
  }))
  const edges: FlowEdge[] = (doc.edges ?? []).map((e) => ({
    id: edgeId(e),
    source: e.from,
    target: e.to,
    label: e.label,
  }))
  return { nodes, edges, viewport: clampViewport(doc.metadata?.canvas?.viewport) }
}

function edgeId(e: Pick<DslEdge, 'from' | 'to'>): string {
  return `e:${e.from}:${e.to}`
}

/** 画布 → DSL:重建 nodes/edges/metadata.canvas,其余字段自 base 透传。 */
export function fromCanvas(state: CanvasState, base: WorkflowDoc): WorkflowDoc {
  const nodes = state.nodes.map((n) => n.data.spec)
  const edges: DslEdge[] = state.edges.map((e) => {
    const out: DslEdge = { from: e.source, to: e.target }
    if (typeof e.label === 'string' && e.label !== '') out.label = e.label
    return out
  })
  const canvas: CanvasMeta = {
    nodes: Object.fromEntries(
      state.nodes.map((n) => [n.id, { position: { x: roundCoord(n.position.x), y: roundCoord(n.position.y) } }]),
    ),
    viewport: clampViewport(state.viewport),
  }
  return {
    dsl: base.dsl,
    name: base.name,
    $comment: base.$comment,
    description: base.description,
    metadata: { canvas },
    inputs: base.inputs,
    edges,
    nodes,
    outputs: base.outputs,
  }
}

// ---- canonical 键序序列化(所有重建只挑已定义键) ----

type Entry = [string, unknown]

function defined(entries: Entry[]): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of entries) {
    if (v !== undefined) out[k] = v
  }
  return out
}

function orderPrompt(p: LlmNode['prompt']): Record<string, unknown> {
  return defined([
    ['system', p.system],
    ['template', p.template],
    ['user_data', p.user_data?.map((u) => defined([['name', u.name], ['ref', u.ref]]))],
  ])
}

function orderBranchCase(c: BranchCase): Record<string, unknown> {
  return defined([
    ['when', defined([['left', c.when.left], ['op', c.when.op], ['right', c.when.right]])],
    ['next', c.next],
  ])
}

function orderBody(b: ParallelBody): Record<string, unknown> {
  const common: Entry[] = [
    ['type', b.type],
    ['title', b.title],
    ['description', b.description],
    ['retry', b.retry],
  ]
  if (b.type === 'llm') {
    return defined([
      ...common,
      ['model', b.model],
      ['temperature', b.temperature],
      ['max_tokens', b.max_tokens],
      ['response_format', b.response_format],
      ['prompt', orderPrompt(b.prompt)],
      ['outputs', b.outputs],
    ])
  }
  return defined([...common, ['tool', b.tool], ['inputs', b.inputs], ['outputs', b.outputs]])
}

function orderNode(n: DslNode): Record<string, unknown> {
  const common: Entry[] = [
    ['id', n.id],
    ['type', n.type],
    ['title', n.title],
    ['description', n.description],
    ['retry', n.retry],
  ]
  switch (n.type) {
    case 'llm':
      return defined([
        ...common,
        ['model', n.model],
        ['temperature', n.temperature],
        ['max_tokens', n.max_tokens],
        ['response_format', n.response_format],
        ['prompt', orderPrompt(n.prompt)],
        ['outputs', n.outputs],
      ])
    case 'tool':
      return defined([...common, ['tool', n.tool], ['inputs', n.inputs], ['outputs', n.outputs]])
    case 'branch':
      return defined([...common, ['cases', n.cases.map(orderBranchCase)], ['default', n.default]])
    case 'approval':
      return defined([
        ...common,
        ['approvers', n.approvers],
        ['mode', n.mode],
        ['timeout_hours', n.timeout_hours],
        ['on_timeout', n.on_timeout],
        ['instructions', n.instructions],
        ['outputs', n.outputs],
      ])
    case 'parallel':
      return defined([
        ...common,
        ['parallel', defined([['foreach', n.parallel.foreach], ['on_failure', n.parallel.on_failure], ['body', orderBody(n.parallel.body)]])],
      ])
  }
}

function orderDoc(doc: WorkflowDoc): Record<string, unknown> {
  const canvas = doc.metadata?.canvas
  return defined([
    ['dsl', doc.dsl],
    ['name', doc.name],
    ['$comment', doc.$comment],
    ['description', doc.description],
    [
      'metadata',
      canvas && {
        canvas: {
          nodes: Object.fromEntries(
            Object.entries(canvas.nodes).map(([id, v]) => [
              id,
              defined([['position', { x: roundCoord(v.position.x), y: roundCoord(v.position.y) }]]),
            ]),
          ),
          viewport: canvas.viewport
            ? defined([
                ['x', roundCoord(canvas.viewport.x)],
                ['y', roundCoord(canvas.viewport.y)],
                ['zoom', canvas.viewport.zoom],
              ])
            : undefined,
        },
      },
    ],
    [
      'inputs',
      doc.inputs?.map((i) =>
        defined([['name', i.name], ['type', i.type], ['required', i.required], ['description', i.description]]),
      ),
    ],
    [
      'edges',
      (doc.edges ?? []).map((e) => defined([['from', e.from], ['to', e.to], ['label', e.label]])),
    ],
    ['nodes', doc.nodes.map(orderNode)],
    ['outputs', doc.outputs],
  ])
}

/** canonical JSON:键序钉死 + 2 空格缩进 + 结尾换行(入库/diff 友好)。 */
export function serializeDoc(doc: WorkflowDoc): string {
  return JSON.stringify(orderDoc(doc), null, 2) + '\n'
}

/**
 * nextNodeId:palette 加节点用。期望名冲突/超长时退到 `<base>_n<i>`;
 * 产物恒满足 schema identifier 词域(基名由 palette 保证合规,此函数
 * 只做唯一性与长度防御)。
 */
export function nextNodeId(base: string, taken: Iterable<string>): string {
  const used = new Set(taken)
  const clean = base.replace(/[^a-z0-9_]/g, '').slice(0, 55) || 'node'
  const stem = /^[a-z]/.test(clean) ? clean : `n_${clean}`
  if (!used.has(stem)) return stem
  for (let i = 2; i < 1000; i++) {
    const candidate = `${stem}_n${i}`
    if (candidate.length <= 64 && !used.has(candidate)) return candidate
  }
  throw new Error('nextNodeId: exhausted')
}

// 类型窄化辅助(测试与组件层共用)。
export function asLlm(n: DslNode): LlmNode {
  return n as LlmNode
}
export function asTool(n: DslNode): ToolNode {
  return n as ToolNode
}
export function asBranch(n: DslNode): BranchNode {
  return n as BranchNode
}
export function asApproval(n: DslNode): ApprovalNode {
  return n as ApprovalNode
}
