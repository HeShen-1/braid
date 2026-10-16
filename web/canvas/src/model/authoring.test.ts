import { describe, expect, it } from 'vitest'
import { canConnect, connectGraph, removeGraphElements, updateGraphNode, type GraphState } from './authoring.ts'
import { fromCanvas, serializeDoc, toCanvas } from './convert.ts'
import type { BranchNode, WorkflowDoc } from './dsl.ts'
import { loadBase } from './testdata.ts'

function branch(graph: GraphState): BranchNode {
  return graph.nodes.find((n) => n.id === 'verdict')!.data.spec as BranchNode
}

function targets(graph: GraphState) {
  return graph.edges.filter((e) => e.source === 'verdict').map((e) => e.target).sort()
}

describe('分支编排事务', () => {
  it.each(['case_0', 'default'])('从 %s 连线同时回写路由,重连替换旧目标并保持唯一边', (handle) => {
    const graph = toCanvas(loadBase('all-node-types'))
    const before = structuredClone(graph)
    const next = connectGraph(graph, { source: 'verdict', target: 'review', sourceHandle: handle })
    expect(handle === 'case_0' ? branch(next).cases[0].next : branch(next).default).toBe('review')
    expect(targets(next)).toEqual(handle === 'case_0' ? ['review', 'revise'] : ['report', 'review'])
    expect(next.edges.find((e) => e.source === 'verdict' && e.target === 'review')?.sourceHandle).toBe(handle)
    expect(connectGraph(next, { source: 'verdict', target: 'review', sourceHandle: handle }).edges).toEqual(next.edges)
    expect(graph).toEqual(before)
  })

  it('两出口同指 A 后仅改一路为 B,另一路的 A 连线保留', () => {
    let graph: GraphState = toCanvas(loadBase('all-node-types'))
    graph = connectGraph(graph, { source: 'verdict', target: 'report', sourceHandle: 'default' })
    expect(branch(graph).cases[0].next).toBe('report')
    expect(branch(graph).default).toBe('report')
    expect(targets(graph)).toEqual(['report'])
    graph = connectGraph(graph, { source: 'verdict', target: 'revise', sourceHandle: 'case_0' })
    expect(branch(graph).cases[0].next).toBe('revise')
    expect(branch(graph).default).toBe('report')
    expect(targets(graph)).toEqual(['report', 'revise'])
    expect(graph.edges.find((e) => e.source === 'verdict' && e.target === 'report')?.sourceHandle).toBe('default')
  })

  it('面板选择/删除中间 case 后出边集合和左移的锚点索引同步', () => {
    let graph: GraphState = toCanvas(loadBase('all-node-types'))
    graph = updateGraphNode(graph, 'verdict', (s) => s.type !== 'branch' ? s : {
      ...s, cases: ['report', 'revise', 'review'].map((next) => ({ ...s.cases[0], next })), default: 'report',
    })
    expect(targets(graph)).toEqual(['report', 'review', 'revise'])
    graph = updateGraphNode(graph, 'verdict', (s) => s.type !== 'branch' ? s : { ...s, cases: s.cases.filter((_, i) => i !== 1) })
    expect(branch(graph).cases.map((c) => c.next)).toEqual(['report', 'review'])
    expect(targets(graph)).toEqual(['report', 'review'])
    expect(graph.edges.find((e) => e.source === 'verdict' && e.target === 'review')?.sourceHandle).toBe('case_1')
  })

  it('删除一条共目标连线清空其所有路由,重连仍可恢复', () => {
    const graph = connectGraph(toCanvas(loadBase('all-node-types')), { source: 'verdict', target: 'report', sourceHandle: 'default' })
    const next = removeGraphElements(graph, [], ['e:verdict:report'])
    expect(branch(next).cases[0].next).toBe('')
    expect(branch(next).default).toBe('')
    expect(targets(next)).toEqual([])
    expect(targets(connectGraph(next, { source: 'verdict', target: 'revise', sourceHandle: 'default' }))).toEqual(['revise'])
  })

  it('删除目标节点不留分支路由或孤儿边,保留其他节点和边', () => {
    const graph = toCanvas(loadBase('all-node-types'))
    const next = removeGraphElements(graph, ['report'], [])
    expect(branch(next).cases[0].next).toBe('')
    expect(branch(next).default).toBe('revise')
    expect(next.nodes.some((n) => n.id === 'report')).toBe(false)
    expect(next.edges.some((e) => e.target === 'report' || e.source === 'report')).toBe(false)
    expect(targets(next)).toEqual(['revise'])
  })

  it('未命名兜底只绑定既有路由;自连/不存在目标均不接受', () => {
    const graph = toCanvas(loadBase('all-node-types'))
    expect(canConnect(graph, { source: 'verdict', target: 'report' })).toBe(true)
    expect(canConnect(graph, { source: 'verdict', target: 'review' })).toBe(false)
    expect(canConnect(graph, { source: 'verdict', target: 'verdict', sourceHandle: 'default' })).toBe(false)
    expect(canConnect(graph, { source: 'verdict', target: 'missing', sourceHandle: 'default' })).toBe(false)
    const next = connectGraph(graph, { source: 'verdict', target: 'report' })
    expect(branch(next)).toEqual(branch(graph))
    expect(next.edges.find((e) => e.source === 'verdict' && e.target === 'report')?.sourceHandle).toBeUndefined()
  })

  it('历史 metadata 锚点与业务路由相反时,打开/无操作保存/改标题均不交换路由', () => {
    const doc = loadBase('with-positions')
    doc.metadata!.canvas!.edges = [
      { from: 'verdict', to: 'report', source_anchor: 'default' },
      { from: 'verdict', to: 'revise', source_anchor: 'case_0' },
    ]
    const original = structuredClone(doc.nodes.find((n) => n.id === 'verdict')) as BranchNode
    const graph = toCanvas(doc)
    const edited = updateGraphNode(graph, 'verdict', (s) => ({ ...s, title: '改标题' }))
    const saved = JSON.parse(serializeDoc(fromCanvas({ ...edited, viewport: graph.viewport }, doc))) as WorkflowDoc
    const savedBranch = saved.nodes.find((n) => n.id === 'verdict') as BranchNode
    expect(savedBranch.cases).toEqual(original.cases)
    expect(savedBranch.default).toBe(original.default)
    expect(saved.metadata!.canvas!.edges).toEqual(doc.metadata!.canvas!.edges)
    expect(fromCanvas(graph, doc).nodes).toEqual(doc.nodes)
  })

  it('非分支连线与节点编辑不改任何路由,保留已有边的标签', () => {
    const graph = toCanvas(loadBase('all-node-types'))
    graph.edges[0].label = '保持标签'
    const next = connectGraph(graph, { source: graph.edges[0].source, target: graph.edges[0].target })
    expect(next.edges[0].label).toBe('保持标签')
    expect(branch(next)).toEqual(branch(graph))
  })
})
