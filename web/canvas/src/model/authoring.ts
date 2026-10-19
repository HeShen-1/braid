// 画布编辑事务:分支的路由字段与出边一起改,导入/导出仍保持原 DSL 契约。
import { connectEdges, handleSide, type CanvasState, type EdgeConnection, type FlowEdge } from './convert.ts'
import { caseAnchorId, DEFAULT_SOURCE_ANCHOR, type BranchNode, type DslNode } from './dsl.ts'

export type GraphState = Pick<CanvasState, 'nodes' | 'edges'>

function routes(spec: BranchNode) {
  return [
    ...spec.cases.map((c, i) => ({ handle: caseAnchorId(i), target: c.next })),
    { handle: DEFAULT_SOURCE_ANCHOR, target: spec.default },
  ]
}

/** 分支目标集合唯一:多条件/默认可去同一节点,顶层仍只保留一条 (from,to) 边。 */
function syncBranchEdges(spec: BranchNode, edges: FlowEdge[]): FlowEdge[] {
  const targets = routes(spec).filter((r) => r.target !== '')
  const outgoing = new Map(edges.filter((e) => e.source === spec.id).map((e) => [e.target, e]))
  const next = [...new Set(targets.map((r) => r.target))].map((target) => {
    const old = outgoing.get(target)
    const matching = targets.filter((r) => r.target === target)
    // 显式未绑定仍是合法画布元数据;失效的命名绑定才重新按当前路由推导。
    const sourceHandle = old !== undefined && (old.sourceHandle === undefined || matching.some((r) => r.handle === old.sourceHandle))
      ? old.sourceHandle
      : matching[0].handle
    return { ...old, id: `e:${spec.id}:${target}`, source: spec.id, target, sourceHandle }
  })
  return [...edges.filter((e) => e.source !== spec.id), ...next]
}

export function updateGraphNode(graph: GraphState, id: string, patch: (spec: DslNode) => DslNode): GraphState {
  const old = graph.nodes.find((n) => n.id === id)?.data.spec
  if (old === undefined) return graph
  const spec = patch(old)
  const changedRoutes = old.type === 'branch' && spec.type === 'branch' &&
    (old.default !== spec.default || old.cases.length !== spec.cases.length || old.cases.some((c, i) => c.next !== spec.cases[i].next))
  return {
    nodes: graph.nodes.map((n) => n.id === id ? { ...n, data: { ...n.data, spec } } : n),
    edges: changedRoutes && spec.type === 'branch' ? syncBranchEdges(spec, graph.edges) : graph.edges,
  }
}

/** 无名兜底出口只可绑定已有路由;新增分支目标必须明确选择条件/默认出口。 */
export function canConnect(graph: GraphState, c: EdgeConnection): boolean {
  if (c.source === c.target || !graph.nodes.some((n) => n.id === c.target)) return false
  if (c.targetHandle != null && handleSide('target', c.targetHandle) === undefined) return false
  const spec = graph.nodes.find((n) => n.id === c.source)?.data.spec
  if (spec === undefined) return false
  if (spec.type !== 'branch') return c.sourceHandle == null || handleSide('source', c.sourceHandle) !== undefined
  return routes(spec).some((r) => c.sourceHandle == null ? r.target === c.target : r.handle === c.sourceHandle)
}

export function connectGraph(graph: GraphState, c: EdgeConnection): GraphState {
  if (!canConnect(graph, c)) return graph
  const next = updateGraphNode(graph, c.source, (spec) => {
    if (spec.type !== 'branch' || !c.sourceHandle) return spec
    if (c.sourceHandle === DEFAULT_SOURCE_ANCHOR) return { ...spec, default: c.target }
    return { ...spec, cases: spec.cases.map((item, i) => caseAnchorId(i) === c.sourceHandle ? { ...item, next: c.target } : item) }
  })
  return { ...next, edges: connectEdges(next.edges, c) }
}

/** 删除连线清掉指向它的分支路由;删除目标节点同时清掉路由与关联边。 */
export function removeGraphElements(graph: GraphState, nodeIds: string[], edgeIds: string[]): GraphState {
  if (nodeIds.length === 0 && edgeIds.length === 0) return graph
  const nodes = new Set(nodeIds)
  const removedEdges = new Set(edgeIds)
  const removed = graph.edges.filter((e) => removedEdges.has(e.id))
  return {
    nodes: graph.nodes.filter((n) => !nodes.has(n.id)).map((n) => {
      const spec = n.data.spec
      if (spec.type !== 'branch') return n
      const targets = new Set([...nodeIds, ...removed.filter((e) => e.source === n.id).map((e) => e.target)])
      if (!routes(spec).some((r) => targets.has(r.target))) return n
      return { ...n, data: { ...n.data, spec: {
        ...spec,
        cases: spec.cases.map((c) => targets.has(c.next) ? { ...c, next: '' } : c),
        default: targets.has(spec.default) ? '' : spec.default,
      } } }
    }),
    edges: graph.edges.filter((e) => !nodes.has(e.source) && !nodes.has(e.target) && !removedEdges.has(e.id)),
  }
}
