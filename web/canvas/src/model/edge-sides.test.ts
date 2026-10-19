import { describe, expect, it } from 'vitest'
import { canConnect, connectGraph, removeGraphElements, updateGraphNode } from './authoring.ts'
import { fromCanvas, reconcileSourceHandles, serializeDoc, toCanvas } from './convert.ts'
import type { EdgeSide, WorkflowDoc } from './dsl.ts'
import { loadBase } from './testdata.ts'

const SOURCES = [
  { side: 'top', handle: 'source_top', stored: 'top' },
  { side: 'right', handle: 'source_right', stored: 'right' },
  { side: 'bottom', handle: undefined, stored: undefined },
  { side: 'left', handle: 'source_left', stored: 'left' },
] as const
const TARGETS = [
  { side: 'top', handle: undefined, stored: undefined },
  { side: 'right', handle: 'target_right', stored: 'right' },
  { side: 'bottom', handle: 'target_bottom', stored: 'bottom' },
  { side: 'left', handle: 'target_left', stored: 'left' },
] as const

function edge(doc: WorkflowDoc, from: string, to: string) {
  return doc.metadata?.canvas?.edges?.find((e) => e.from === from && e.to === to)
}

function canonicalRound(doc: WorkflowDoc): WorkflowDoc {
  return JSON.parse(serializeDoc(fromCanvas(toCanvas(doc), doc))) as WorkflowDoc
}

describe('UX-F2 物理方向与分支语义独立', () => {
  it.each(SOURCES.flatMap((source) => TARGETS.map((target) => ({ source, target }))))(
    '普通节点 $source.side → $target.side 保存重开;默认方向省略,业务字段保真', ({ source, target }) => {
      const doc = loadBase('edge-sides')
      const state = toCanvas(doc)
      state.edges = [{ id: 'e:read_doc:parse', source: 'read_doc', target: 'parse', sourceHandle: source.handle, targetHandle: target.handle, label: '保留标签' }]
      const saved = fromCanvas(state, doc)
      const record = edge(saved, 'read_doc', 'parse')
      if (source.stored === undefined && target.stored === undefined) expect(record).toBeUndefined()
      else expect(record).toEqual({ from: 'read_doc', to: 'parse',
        ...(source.stored === undefined ? {} : { source_side: source.stored }),
        ...(target.stored === undefined ? {} : { target_side: target.stored }),
      })
      const reopened = toCanvas(JSON.parse(serializeDoc(saved)))
      expect(reopened.edges[0]).toMatchObject({ sourceHandle: source.handle, targetHandle: target.handle, label: '保留标签' })
      expect(saved.nodes).toEqual(doc.nodes)
      expect(saved.inputs).toEqual(doc.inputs)
      expect(saved.outputs).toEqual(doc.outputs)
      expect(canonicalRound(saved)).toEqual(JSON.parse(serializeDoc(saved)))
    },
  )

  it.each(['absent', 'unbound', 'stale'].flatMap((mode) => TARGETS.map((target) => ({ mode, target }))))(
    '旧 $mode 绑定 + 新 $target.side 目标保持原裁决与业务路由', ({ mode, target }) => {
      const doc = loadBase('with-positions')
      const canvas = doc.metadata!.canvas!
      canvas.edges = canvas.edges!.filter((e) => !(e.from === 'verdict' && e.to === 'report'))
      if (mode !== 'absent') canvas.edges.push({ from: 'verdict', to: 'report', ...(mode === 'stale' ? { source_anchor: 'case_7' } : {}) })
      const state = toCanvas(doc)
      const original = state.edges.find((e) => e.id === 'e:verdict:report')!
      expect(original.sourceHandle).toBe(mode === 'unbound' ? undefined : 'case_0')
      original.targetHandle = target.handle
      const saved = fromCanvas(state, doc)
      const reopened = toCanvas(JSON.parse(serializeDoc(saved))).edges.find((e) => e.id === original.id)!
      expect(reopened.sourceHandle).toBe(mode === 'unbound' ? undefined : 'case_0')
      expect(reopened.targetHandle).toBe(target.handle)
      expect(saved.nodes).toEqual(doc.nodes)
      expect(edge(saved, 'verdict', 'report')).not.toHaveProperty('source_side')
    },
  )

  it.each(TARGETS)('内部陈旧源锚 + $side 目标方向不会误变显式未绑定', (target) => {
    const doc = loadBase('with-positions')
    const state = toCanvas(doc)
    const current = state.edges.find((e) => e.id === 'e:verdict:report')!
    current.sourceHandle = 'case_7'
    current.targetHandle = target.handle
    const saved = fromCanvas(state, doc)
    const reopened = toCanvas(saved).edges.find((e) => e.id === current.id)!
    expect(reopened.sourceHandle).toBe('case_0')
    expect(reopened.targetHandle).toBe(target.handle)
    expect(saved.nodes).toEqual(doc.nodes)
    if (target.stored === undefined) expect(edge(saved, 'verdict', 'report')).toBeUndefined()
    else expect(edge(saved, 'verdict', 'report')).toEqual({ from: 'verdict', to: 'report', source_anchor: 'case_0', target_side: target.stored })
  })

  it.each([undefined, 'case_7'])('历史非路由目标仍落兜底,方向不制造 next/default (%s)', (sourceHandle) => {
    const doc = loadBase('with-positions')
    const state = toCanvas(doc)
    state.edges = [{ id: 'e:verdict:review', source: 'verdict', target: 'review', sourceHandle, targetHandle: 'target_left' }]
    const saved = fromCanvas(state, doc)
    expect(edge(saved, 'verdict', 'review')).toEqual({ from: 'verdict', to: 'review', target_side: 'left' })
    expect(toCanvas(saved).edges[0].sourceHandle).toBeUndefined()
    expect(toCanvas(saved).edges[0].targetHandle).toBe('target_left')
    expect(saved.nodes).toEqual(doc.nodes)
  })

  it.each(['top', 'right', 'bottom', 'left'] as const)('branch 源忽略并清理 source_side=%s,保留语义与目标方向', (side) => {
    const doc = loadBase('with-positions')
    doc.metadata!.canvas!.edges![0] = { from: 'verdict', to: 'report', source_anchor: 'case_0', source_side: side, target_side: 'left' }
    const state = toCanvas(doc)
    expect(state.edges.find((e) => e.id === 'e:verdict:report')).toMatchObject({ sourceHandle: 'case_0', targetHandle: 'target_left' })
    const saved = fromCanvas(state, doc)
    expect(edge(saved, 'verdict', 'report')).toEqual({ from: 'verdict', to: 'report', source_anchor: 'case_0', target_side: 'left' })
    expect(saved.nodes).toEqual(doc.nodes)
  })

  it.each(['diagonal', '', 'RIGHT', 'source_right', null, 1, {}])('非法方向 %j 导入不留下不存在的物理手柄', (bad) => {
    const doc = loadBase('with-positions')
    doc.metadata!.canvas!.edges!.push({ from: 'review', to: 'verdict', source_side: bad as EdgeSide, target_side: bad as EdgeSide })
    const state = toCanvas(doc)
    expect(state.edges.find((e) => e.id === 'e:review:verdict')).toMatchObject({ sourceHandle: undefined, targetHandle: undefined })
    expect(edge(fromCanvas(state, doc), 'review', 'verdict')).toBeUndefined()
  })

  it('缺省 bottom/top 不冗余落盘,字段规范顺序仍为语义锚在方向之前', () => {
    const doc = loadBase('edge-sides')
    doc.metadata!.canvas!.edges!.push({ from: 'revise', to: 'report', source_side: 'bottom', target_side: 'top' })
    const saved = canonicalRound(doc)
    expect(edge(saved, 'revise', 'report')).toBeUndefined()
    expect(Object.keys(edge(saved, 'verdict', 'report')!)).toEqual(['from', 'to', 'source_anchor', 'target_side'])
    expect(Object.keys(edge(saved, 'read_doc', 'parse')!)).toEqual(['from', 'to', 'source_side', 'target_side'])
  })

  it('源锚对账保留普通物理手柄,只清不存在或错误类型的源 id', () => {
    const state = toCanvas(loadBase('edge-sides'))
    expect(reconcileSourceHandles(state.nodes, state.edges)).toBeNull()
    state.edges.find((e) => e.id === 'e:review:verdict')!.sourceHandle = 'case_0'
    const fixed = reconcileSourceHandles(state.nodes, state.edges)!
    expect(fixed.find((e) => e.id === 'e:review:verdict')!.sourceHandle).toBeUndefined()
    expect(fixed.find((e) => e.id === 'e:read_doc:parse')!.sourceHandle).toBe('source_right')
    expect(fixed.find((e) => e.id === 'e:verdict:report')!.sourceHandle).toBe('case_0')
  })

  it('普通物理连线不改业务节点,同一对重绑保标签与唯一边;标题/分支去向编辑保目标方向', () => {
    const doc = loadBase('edge-sides')
    const state = toCanvas(doc)
    state.edges.find((e) => e.id === 'e:review:verdict')!.label = '进入分支'
    const next = connectGraph(state, { source: 'review', target: 'verdict', sourceHandle: 'source_left', targetHandle: 'target_right' })
    expect(next.nodes.map((n) => n.data.spec)).toEqual(doc.nodes)
    expect(next.edges.filter((e) => e.id === 'e:review:verdict')).toHaveLength(1)
    expect(next.edges.find((e) => e.id === 'e:review:verdict')).toMatchObject({ sourceHandle: 'source_left', targetHandle: 'target_right', label: '进入分支' })
    const edited = updateGraphNode(next, 'verdict', (s) => s.type !== 'branch' ? s : { ...s, title: '标题更新', default: 'report' })
    expect(edited.edges.find((e) => e.id === 'e:verdict:report')!.targetHandle).toBe('target_right')
    expect(edge(fromCanvas({ ...edited, viewport: state.viewport }, doc), 'review', 'verdict')).toEqual({ from: 'review', to: 'verdict', source_side: 'left', target_side: 'right' })
  })

  it('物理手柄不冒充分支 case/default,非法目标手柄也不连线', () => {
    const state = toCanvas(loadBase('edge-sides'))
    for (const bad of ['source_right', 'source_top', 'case_7', '']) {
      const c = { source: 'verdict', target: 'report', sourceHandle: bad }
      expect(canConnect(state, c)).toBe(false)
      expect(connectGraph(state, c)).toBe(state)
    }
    for (const source of ['review', 'verdict']) {
      const c = { source, target: 'report', targetHandle: 'source_left' }
      expect(canConnect(state, c)).toBe(false)
    }
    expect(canConnect(state, { source: 'review', target: 'verdict', sourceHandle: 'case_0' })).toBe(false)
    expect(canConnect(state, { source: 'verdict', target: 'report', targetHandle: 'target_bottom' })).toBe(true)
  })

  it.each(['edge', 'node'])('删除 %s 清掉方向元数据,不留陈旧绑定', (kind) => {
    const doc = loadBase('edge-sides')
    const state = toCanvas(doc)
    const next = removeGraphElements(state, kind === 'node' ? ['parse'] : [], kind === 'edge' ? ['e:read_doc:parse'] : [])
    const saved = fromCanvas({ ...next, viewport: state.viewport }, doc)
    expect(edge(saved, 'read_doc', 'parse')).toBeUndefined()
    if (kind === 'node') expect(saved.metadata?.canvas?.edges?.some((e) => e.from === 'parse' || e.to === 'parse')).toBe(false)
    expect(edge(saved, 'review', 'verdict')).toEqual(edge(doc, 'review', 'verdict'))
  })
})
