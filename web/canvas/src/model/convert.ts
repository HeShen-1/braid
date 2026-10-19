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
  CanvasEdgeAnchor,
  CanvasMeta,
  DslEdge,
  DslNode,
  EdgeSide,
  LlmNode,
  ParallelBody,
  Point,
  ToolNode,
  Viewport,
  WorkflowDoc,
} from './dsl.ts'
import { DEFAULT_SOURCE_ANCHOR, EDGE_SIDES, caseAnchorId } from './dsl.ts'

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
  /** branch 为 case_<i>/default;普通节点为 source_<side>,缺省 bottom 无 id。 */
  sourceHandle?: string
  targetHandle?: string
  label?: string
}

export interface CanvasState {
  nodes: FlowNode[]
  edges: FlowEdge[]
  viewport: Viewport
}

/** 默认源 bottom / 目标 top 保留无 id 手柄且 DOM 首位,旧边无需迁移。 */
export function sideHandle(type: 'source' | 'target', side: unknown): string | undefined {
  const fallback = type === 'source' ? 'bottom' : 'top'
  return EDGE_SIDES.some((s) => s === side) && side !== fallback ? `${type}_${side}` : undefined
}

/** 只识别实际存在的非默认物理手柄;未知/过期 id 不冒充方向落盘。 */
export function handleSide(type: 'source' | 'target', handle: string | undefined): EdgeSide | undefined {
  return handle === undefined ? undefined : EDGE_SIDES.find((s) => sideHandle(type, s) === handle)
}

/** 节点出口上的一个可连点(id 进 schema $defs/sourceAnchor 词域)。 */
export interface SourceAnchor {
  id: string
  label: string
  /** 该出口还没声明去向(cases[i].next / default 为空)—— 作者事实,不是校验裁定。 */
  blank: boolean
}

/**
 * 节点源锚点集合(按节点语义推导,单一真源 = spec):
 *   - branch:每条 case 一个 case_<i>(下标对齐 cases 序)+ default;
 *   - 其余四类节点:DSL 语义上只有一个出口(approval 的通过/驳回不是路由,
 *     驳回即执行失败)→ 空集 = 单缺省手柄,不虚构锚点。
 * 集合推导而不落盘:锚点是 spec 的函数,写进 metadata 只会造出第二个真源;
 * metadata.canvas.edges 的 source_anchor 只持久化语义绑定;物理方向由
 * source_side/target_side 分开承载,不改变本集合。
 */
export function sourceAnchors(spec: DslNode): SourceAnchor[] {
  if (spec.type !== 'branch') return []
  const cases = spec.cases ?? []
  return [
    ...cases.map((c, i) => ({
      id: caseAnchorId(i),
      label: `#${i + 1} → ${c.next || '待填'}`,
      blank: c.next === '',
    })),
    { id: DEFAULT_SOURCE_ANCHOR, label: `默认 → ${spec.default || '待填'}`, blank: spec.default === '' },
  ]
}

function hasAnchor(spec: DslNode | undefined, id: string): boolean {
  return spec !== undefined && sourceAnchors(spec).some((a) => a.id === id)
}

/**
 * 锚点绑定对账:面板增删 cases 之后,原先绑在已消失锚点上的边会悬空 ——
 * React Flow 按 id 精确找手柄,找不到就不画那条边(文档里还在,画布上消失)。
 * 清掉失效绑定让边回落到缺省/回退手柄;无需变更时返回 null(调用方据此
 * 跳过 setState,避免无谓重渲)。
 */
export function reconcileSourceHandles(nodes: FlowNode[], edges: FlowEdge[]): FlowEdge[] | null {
  if (!edges.some((e) => typeof e.sourceHandle === 'string')) return null
  // 锚点 id 集合一次算齐:本函数挂在节点变更(拖动每帧也走)上,别按边重算 spec。
  const specs = new Map(nodes.map((n) => [n.id, n.data.spec]))
  const anchorIds = new Map(nodes.map((n) => [n.id, new Set(sourceAnchors(n.data.spec).map((a) => a.id))]))
  let dirty = false
  const next = edges.map((e) => {
    const spec = specs.get(e.source)
    const valid = spec?.type === 'branch' ? anchorIds.get(e.source)?.has(e.sourceHandle ?? '') :
      spec !== undefined && handleSide('source', e.sourceHandle) !== undefined
    if (typeof e.sourceHandle !== 'string' || valid) return e
    dirty = true
    const cleared: FlowEdge = { ...e }
    delete cleared.sourceHandle
    return cleared
  })
  return dirty ? next : null
}

/** 无绑定时按语义推导:目标命中某条 case.next → 该 case 锚点;命中 default → default。 */
function derivedAnchor(spec: DslNode, target: string): string | undefined {
  if (spec.type !== 'branch') return undefined
  const cases = spec.cases ?? []
  const i = cases.findIndex((c) => c.next !== '' && c.next === target)
  if (i >= 0) return caseAnchorId(i)
  return spec.default !== '' && spec.default === target ? DEFAULT_SOURCE_ANCHOR : undefined
}

/**
 * 边端点的源锚点裁决(三分支,顺序即优先级):
 *   1. 该边在 metadata.canvas.edges 里**没有记录** → 按语义推导(老文档/外部文档
 *      因此开箱就有锚点连线);
 *   2. 有记录且带 source_anchor,且该锚点真在当前 spec 的锚点集里 → 用它(作者显式绑定);
 *   3. 有记录但省略 source_anchor → **显式未绑定**(作者把边落在兜底手柄上),
 *      不做推导 —— 否则重开一次就被改回语义锚点(独立复核 MEDIUM-2)。
 * 记录里的锚点已不存在(改过 cases)= 悬空,退回语义推导。
 */
function resolveSourceHandle(
  spec: DslNode | undefined,
  target: string,
  record: { present: boolean; anchor: string | undefined },
): string | undefined {
  if (spec === undefined || sourceAnchors(spec).length === 0) return undefined
  if (!record.present) return derivedAnchor(spec, target)
  if (record.anchor === undefined) return undefined
  return hasAnchor(spec, record.anchor) ? record.anchor : derivedAnchor(spec, target)
}

// 边身份 = (from,to):顶层 edges 的二元组唯一性由 T1.2 语义校验保证
// (validate.go IssueDuplicateEdge);identifier 词域不含 ':' ⇒ 拼接无碰撞。
function edgeKey(from: string, to: string): string {
  return `${from}:${to}`
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
  // T21.4 ③ 源锚点:落盘绑定优先(须真在该节点的锚点集内,悬空绑定丢弃),
  // 否则按 cases[].next/default 推导 —— 老文档/外部文档因此也能开出锚点连线。
  // 「有没有记录」本身是信息:记录在场 = 作者表过态,省略 source_anchor 即显式
  // 未绑定(不推导),见 resolveSourceHandle。
  const records = new Map((doc.metadata?.canvas?.edges ?? []).map((a) => [edgeKey(a.from, a.to), a]))
  const specs = new Map(doc.nodes.map((n) => [n.id, n]))
  const edges: FlowEdge[] = (doc.edges ?? []).map((e) => {
    const record = records.get(edgeKey(e.from, e.to))
    const spec = specs.get(e.from)
    const sourceHandle = spec?.type === 'branch' ? resolveSourceHandle(spec, e.to, {
      present: record !== undefined,
      anchor: typeof record?.source_anchor === 'string' && record.source_anchor !== '' ? record.source_anchor : undefined,
    }) : sideHandle('source', record?.source_side)
    const targetHandle = sideHandle('target', record?.target_side)
    return { id: edgeId(e), source: e.from, target: e.to, sourceHandle, targetHandle, label: e.label }
  })
  return { nodes, edges, viewport: clampViewport(doc.metadata?.canvas?.viewport) }
}

function edgeId(e: Pick<DslEdge, 'from' | 'to'>): string {
  return `e:${e.from}:${e.to}`
}

/** 一次连线请求(RF Connection 的结构子集:不引 React Flow 类型)。 */
export interface EdgeConnection {
  source: string
  target: string
  sourceHandle?: string | null
  targetHandle?: string | null
}

/**
 * 连线落库(画布唯一入口):同一对节点只留一条边 —— 顶层 edges 的 (from,to)
 * 唯一性由 T1.2 语义校验保证(重复边 IssueDuplicateEdge 拒),并行边没有 DSL
 * 语义。再从别的锚点连一次 = **改绑**该边的源锚点(既不翻倍,也不静默丢掉
 * 新锚点)。边 id 与 toCanvas 同源(`e:from:to`),不引 React Flow 的 id 方案。
 */
export function connectEdges(edges: FlowEdge[], c: EdgeConnection): FlowEdge[] {
  const dup = edges.find((e) => e.source === c.source && e.target === c.target)
  const sourceHandle = c.sourceHandle ?? undefined
  const targetHandle = c.targetHandle ?? undefined
  if (dup !== undefined) {
    return edges.map((e) => (e === dup ? { ...e, sourceHandle, targetHandle } : e))
  }
  return [...edges, { id: `e:${c.source}:${c.target}`, source: c.source, target: c.target, sourceHandle, targetHandle }]
}

/** 画布 → DSL:重建 nodes/edges/metadata.canvas,其余字段自 base 透传。 */
export function fromCanvas(state: CanvasState, base: WorkflowDoc): WorkflowDoc {
  const nodes = state.nodes.map((n) => n.data.spec)
  const specs = new Map(state.nodes.map((n) => [n.id, n.data.spec]))
  // branch 保留 T21.4 ③ 的「记录在场 = 已表态」;普通节点仅在非默认
  // 方向时落记录。默认 bottom/top 不冗余写,语义锚点不重载为物理方向。
  const anchors: CanvasEdgeAnchor[] = []
  const edges: DslEdge[] = state.edges.map((e) => {
    const out: DslEdge = { from: e.source, to: e.target }
    if (typeof e.label === 'string' && e.label !== '') out.label = e.label
    const spec = specs.get(e.source)
    const sourceSide = spec?.type === 'branch' ? undefined : handleSide('source', e.sourceHandle)
    const targetSide = handleSide('target', e.targetHandle)
    const record: CanvasEdgeAnchor = { from: e.source, to: e.target }
    if (spec?.type === 'branch') {
      const h = e.sourceHandle
      if (h !== undefined && hasAnchor(spec, h)) record.source_anchor = h
      else if (h !== undefined) {
        // 陈旧锚点原本省整条记录,下次导入按路由推导。若要保留新目标方向,
        // 需在同一记录中写回原应推导的语义锚,不能误变「显式未绑定」。
        if (targetSide === undefined) return out
        const derived = derivedAnchor(spec, e.target)
        if (derived !== undefined) record.source_anchor = derived
      }
    }
    if (sourceSide !== undefined) record.source_side = sourceSide
    if (targetSide !== undefined) record.target_side = targetSide
    if (spec?.type === 'branch' || sourceSide !== undefined || targetSide !== undefined) anchors.push(record)
    return out
  })
  const canvas: CanvasMeta = {
    nodes: Object.fromEntries(
      state.nodes.map((n) => [n.id, { position: { x: roundCoord(n.position.x), y: roundCoord(n.position.y) } }]),
    ),
    viewport: clampViewport(state.viewport),
  }
  // 无绑定/自定义方向不落 edges 键:旧缺省文档导出字节不变。
  if (anchors.length > 0) canvas.edges = anchors
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
        ['parallel', defined([
          ['foreach', n.parallel.foreach],
          ['on_failure', n.parallel.on_failure],
          ['min_success', n.parallel.min_success],
          ['body', orderBody(n.parallel.body)],
        ])],
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
          // 键序 = schema 声明序(nodes/viewport/edges);空表不落键。
          edges:
            (canvas.edges ?? []).length > 0
              ? (canvas.edges ?? []).map((a) =>
                  defined([['from', a.from], ['to', a.to], ['source_anchor', a.source_anchor], ['source_side', a.source_side], ['target_side', a.target_side]]),
                )
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
