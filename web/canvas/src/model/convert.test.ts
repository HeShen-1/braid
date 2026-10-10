import { describe, expect, it } from 'vitest'
import { defaultSpec } from '../defaults.ts'
import type { DslNode, NodeType, WorkflowDoc } from './dsl.ts'
import { SOURCE_ANCHOR_PATTERN } from './dsl.ts'
import {
  autoLayout,
  clampViewport,
  connectEdges,
  fromCanvas,
  nextNodeId,
  reconcileSourceHandles,
  serializeDoc,
  sourceAnchors,
  toCanvas,
} from './convert.ts'
import { loadBase } from './testdata.ts'

describe('toCanvas(DSL → 画布)', () => {
  it('metadata.canvas 位置齐全时逐节点采用', () => {
    const doc = loadBase('with-positions')
    const cs = toCanvas(doc)
    expect(cs.nodes.map((n) => n.id)).toEqual(['read_doc', 'parse', 'fan', 'review', 'verdict', 'revise', 'report'])
    expect(cs.nodes.find((n) => n.id === 'revise')!.position).toEqual({ x: 1340, y: 200 })
    expect(cs.viewport).toEqual({ x: 0, y: 0, zoom: 0.85 })
    // spec 引用即文档节点对象(透传契约:面板改 spec = 改导出内容)。
    expect(cs.nodes[0].data.spec).toBe(doc.nodes[0])
  })

  it('缺 metadata.canvas 时整体 autoLayout:位置唯一、入度 0 在第 0 列', () => {
    const cs = toCanvas(loadBase('all-node-types'))
    const seen = new Set(cs.nodes.map((n) => `${n.position.x},${n.position.y}`))
    expect(seen.size).toBe(cs.nodes.length)
    const readDoc = cs.nodes.find((n) => n.id === 'read_doc')!
    expect(readDoc.position).toEqual({ x: 40, y: 40 }) // 唯一入度 0 → 第 0 层首个
    expect(cs.viewport).toEqual({ x: 0, y: 0, zoom: 1 }) // 缺省视口
  })

  it('边映射:RF 形状 + 确定性 id', () => {
    const cs = toCanvas(loadBase('with-positions'))
    expect(cs.edges[0]).toEqual({ id: 'e:read_doc:parse', source: 'read_doc', target: 'parse', label: undefined })
  })
})

describe('autoLayout(分层)', () => {
  it('分支汇聚:report 深于 verdict,revise 与 report 同层不同行', () => {
    const doc = loadBase('all-node-types')
    const layout = autoLayout(doc)
    expect(layout.get('read_doc')!.x).toBeLessThan(layout.get('parse')!.x)
    // 最长路径分层:verdict < revise < report(revise→report 边使 report 更深)。
    expect(layout.get('verdict')!.x).toBeLessThan(layout.get('revise')!.x)
    expect(layout.get('revise')!.x).toBeLessThan(layout.get('report')!.x)
    // 各层首行:revise 与 report 都是其层内唯一节点。
    expect(layout.get('revise')!.y).toBe(layout.get('report')!.y)
  })

  it('环防御:不无限递归,仍产出全量位置', () => {
    const doc: WorkflowDoc = {
      dsl: '1.0',
      name: 'cycle',
      nodes: [
        { id: 'a', type: 'llm', prompt: { template: 't' } },
        { id: 'b', type: 'llm', prompt: { template: 't' } },
      ],
      edges: [
        { from: 'a', to: 'b' },
        { from: 'b', to: 'a' },
      ],
    }
    const layout = autoLayout(doc)
    expect(layout.size).toBe(2)
  })
})

describe('fromCanvas(画布 → DSL)往返', () => {
  it('带位置文档:结构等价往返(位置/视口/全部专有字段)', () => {
    const doc = loadBase('with-positions')
    const round = fromCanvas(toCanvas(doc), doc)
    expect(JSON.parse(serializeDoc(round))).toEqual(doc)
  })

  it('无位置文档:导出补全 canvas 元数据,位置取整', () => {
    const doc = loadBase('all-node-types')
    const round = fromCanvas(toCanvas(doc), doc)
    for (const n of round.nodes) {
      expect(round.metadata?.canvas?.nodes[n.id]?.position).toBeDefined()
    }
    expect(round.metadata?.canvas?.viewport?.zoom).toBe(1)
  })

  it('parallel 嵌套结构逐字段保真(foreach/on_failure/body 无 id)', () => {
    const doc = loadBase('all-node-types')
    const round = fromCanvas(toCanvas(doc), doc)
    const fan = round.nodes.find((n) => n.id === 'fan')!
    if (fan.type !== 'parallel') throw new Error('unreachable')
    expect(fan.parallel.foreach).toBe('{{inputs.items}}')
    expect(fan.parallel.on_failure).toBe('wait_all')
    expect(fan.parallel.body).toEqual({
      type: 'llm',
      // body 无 title:schema oneOf(llm|tool) 精简节点面,公共字段仅
      // type/retry(title/description 不入——Go 契约测试抓过,锚死)。
      prompt: { template: '评审以下内容并给出结论。', user_data: [{ name: 'piece', ref: '{{item}}' }] },
      outputs: { verdict: 'string' },
    })
  })

  it('被删节点的陈旧位置条目随之消失;新增节点位置写入', () => {
    const doc = loadBase('with-positions')
    const cs = toCanvas(doc)
    cs.nodes = cs.nodes.filter((n) => n.id !== 'revise')
    cs.nodes.push({
      id: 'extra',
      type: 'llm',
      position: { x: 10.6, y: -3.2 },
      data: { spec: { id: 'extra', type: 'llm', prompt: { template: 't' } } },
    })
    const round = fromCanvas(cs, doc)
    expect(round.metadata?.canvas?.nodes['revise']).toBeUndefined()
    expect(round.metadata?.canvas?.nodes['extra']).toEqual({ position: { x: 11, y: -3 } })
    expect(round.nodes.map((n) => n.id)).not.toContain('revise')
  })

  it('edge label:非空串保留,空串丢弃', () => {
    const doc = loadBase('minimal-llm')
    const cs = toCanvas(doc)
    cs.edges = [
      { id: 'e:only:only2', source: 'only', target: 'only2', label: '' },
      { id: 'e:only2:only', source: 'only2', target: 'only', label: '已批' },
    ]
    const round = fromCanvas(cs, doc)
    expect(round.edges).toEqual([
      { from: 'only', to: 'only2' },
      { from: 'only2', to: 'only', label: '已批' },
    ])
  })
})

describe('serializeDoc(canonical 键序)', () => {
  it('字节稳定:两次序列化相等,首键 dsl,结尾换行', () => {
    const doc = loadBase('with-positions')
    const a = serializeDoc(fromCanvas(toCanvas(doc), doc))
    const b = serializeDoc(fromCanvas(toCanvas(doc), doc))
    expect(a).toBe(b)
    expect(a.startsWith('{\n  "dsl":')).toBe(true)
    expect(a.endsWith('\n')).toBe(true)
  })

  it('canonical 序:任何导入键序导出同一字节(MetaInFirst 不可依赖)', () => {
    const doc = loadBase('with-positions')
    const shuffled = JSON.parse(
      JSON.stringify({
        outputs: doc.outputs,
        nodes: doc.nodes,
        edges: doc.edges,
        inputs: doc.inputs,
        metadata: doc.metadata,
        description: doc.description,
        name: doc.name,
        dsl: doc.dsl,
      }),
    ) as WorkflowDoc
    expect(serializeDoc(shuffled)).toBe(serializeDoc(doc))
  })
})

describe('clampViewport / nextNodeId', () => {
  it('zoom 夹 0.1..4,x/y 取整', () => {
    expect(clampViewport({ x: 12.7, y: -0.2, zoom: 9 })).toEqual({ x: 13, y: 0, zoom: 4 })
    expect(clampViewport({ x: 0, y: 0, zoom: 0.01 })).toEqual({ x: 0, y: 0, zoom: 0.1 })
    expect(clampViewport(undefined)).toEqual({ x: 0, y: 0, zoom: 1 })
  })

  it('nextNodeId:期望名空闲即用;冲突递增;产物合规', () => {
    const taken = ['llm', 'llm_n2']
    expect(nextNodeId('tool', taken)).toBe('tool')
    expect(nextNodeId('llm', taken)).toBe('llm_n3')
    const id = nextNodeId('BAD NAME!', taken)
    expect(id).toMatch(/^[a-z][a-z0-9_]{0,63}$/)
  })
})

describe('审查修正回归', () => {
  it('palette 默认 parallel spec:on_failure 空串不入序列化(schema 词域)', () => {
    const doc: WorkflowDoc = { dsl: '1.0', name: 't', nodes: [defaultSpec('parallel', 'fan')], edges: [] }
    const out = serializeDoc(fromCanvas(toCanvas(doc), doc))
    expect(out).not.toContain('"on_failure": ""')
    expect(out).not.toContain('"on_failure":""')
  })

  it('$comment 顶层合法字段往返透传(不静默丢字段)', () => {
    const doc = loadBase('minimal-llm')
    doc.$comment = '作者注释'
    const round = fromCanvas(toCanvas(doc), doc)
    expect(round.$comment).toBe('作者注释')
    expect(serializeDoc(round)).toContain('"$comment": "作者注释"')
  })

  it('roundCoord 防御:NaN/Inf 位置归零(JSON.stringify 会编 null 违 schema)', () => {
    const doc = loadBase('minimal-llm')
    const cs = toCanvas(doc)
    cs.nodes[0].position = { x: Number.NaN, y: Number.POSITIVE_INFINITY }
    const round = fromCanvas(cs, doc)
    expect(round.metadata?.canvas?.nodes['only']).toEqual({ position: { x: 0, y: 0 } })
  })
})

// T21.4 ③ 多锚点连线:锚点集合按节点语义从 spec 推导(branch = case_<i> + default),
// metadata.canvas.edges 只持久化「哪条边接在哪个锚点上」;词域真源在 schema.json。
describe('T21.4 ③ 源锚点(branch 多锚点)', () => {
  const branchSpec = (caseCount: number, routed: boolean): DslNode => ({
    id: 'decide',
    type: 'branch',
    cases: Array.from({ length: caseCount }, (_, i) => ({
      when: { left: '{{inputs.x}}', op: 'eq', right: i },
      next: routed ? `n${i}` : '',
    })),
    default: routed ? 'fallback' : '',
  })

  it('sourceAnchors:branch = case_<i>(下标对齐 cases 序)+ default;其余四类节点不虚构锚点', () => {
    const spec = branchSpec(2, false)
    if (spec.type !== 'branch') throw new Error('unreachable')
    spec.cases[0].next = 'pass'
    expect(sourceAnchors(spec)).toEqual([
      { id: 'case_0', label: '#1 → pass', blank: false },
      { id: 'case_1', label: '#2 → 待填', blank: true },
      { id: 'default', label: '默认 → 待填', blank: true },
    ])
    for (const t of ['llm', 'tool', 'approval', 'parallel'] as NodeType[]) {
      expect(sourceAnchors(defaultSpec(t, 'n'))).toEqual([])
    }
  })

  it('锚点词域 = schema $defs/sourceAnchor 镜像:任意 cases 数的推导产物全在词域内', () => {
    for (let n = 0; n <= 70; n++) {
      const anchors = sourceAnchors(branchSpec(n, true))
      expect(anchors).toHaveLength(n + 1)
      for (const a of anchors) expect(a.id).toMatch(SOURCE_ANCHOR_PATTERN)
    }
    // 反锚:词域外形态必须不被镜像放行(放宽正则 = 导出撞发布校验,Go 侧才有第二道门)
    for (const bad of ['else', 'case_', 'case_1x', 'Case_0', 'ok', 'default_']) {
      expect(bad).not.toMatch(SOURCE_ANCHOR_PATTERN)
    }
  })

  it('toCanvas:落盘绑定优先;悬空绑定丢弃并回退语义推导', () => {
    const doc = loadBase('with-positions')
    const bound = toCanvas(doc)
    expect(bound.edges.find((e) => e.id === 'e:verdict:report')?.sourceHandle).toBe('case_0')
    expect(bound.edges.find((e) => e.id === 'e:verdict:revise')?.sourceHandle).toBe('default')

    // 悬空绑定:case_7 在这个 branch(1 条 case)上不存在 ⇒ 不许当真,回退推导。
    const stale = JSON.parse(JSON.stringify(doc)) as WorkflowDoc
    stale.metadata!.canvas!.edges![0].source_anchor = 'case_7'
    expect(toCanvas(stale).edges.find((e) => e.id === 'e:verdict:report')?.sourceHandle).toBe('case_0')
  })

  it('toCanvas:无绑定文档按 cases[].next/default 推导;非 branch 源节点不虚构锚点', () => {
    const cs = toCanvas(loadBase('all-node-types'))
    expect(cs.edges.find((e) => e.source === 'verdict' && e.target === 'report')?.sourceHandle).toBe('case_0')
    expect(cs.edges.find((e) => e.source === 'verdict' && e.target === 'revise')?.sourceHandle).toBe('default')
    expect(cs.edges.find((e) => e.id === 'e:review:verdict')?.sourceHandle).toBeUndefined()
  })

  it('fromCanvas:绑定落 metadata.canvas.edges;悬空 sourceHandle 不落盘', () => {
    const doc = loadBase('all-node-types')
    expect(fromCanvas(toCanvas(doc), doc).metadata?.canvas?.edges).toEqual([
      { from: 'verdict', to: 'report', source_anchor: 'case_0' },
      { from: 'verdict', to: 'revise', source_anchor: 'default' },
    ])
    // approval 节点没有锚点:非 branch 源节点上的 sourceHandle 属画布内部态,不冒充 DSL 语义。
    const cs = toCanvas(doc)
    cs.edges = [{ id: 'e:review:verdict', source: 'review', target: 'verdict', sourceHandle: 'case_0' }]
    expect(fromCanvas(cs, doc).metadata?.canvas?.edges).toBeUndefined()
  })

  it('显式落在兜底手柄 = 落空绑定记录,重开不被语义推导改回去(独立复核 MEDIUM-2)', () => {
    const doc = loadBase('all-node-types')
    const cs = toCanvas(doc)
    // 把 verdict→revise(语义上是 default 去向)改接到兜底手柄:sourceHandle 缺席
    cs.edges = cs.edges.map((e) => (e.id === 'e:verdict:revise' ? { ...e, sourceHandle: undefined } : e))
    const round = fromCanvas(cs, doc)
    expect(round.metadata?.canvas?.edges).toEqual([
      { from: 'verdict', to: 'report', source_anchor: 'case_0' },
      { from: 'verdict', to: 'revise' },
    ])
    // 记录在场 + 省略 source_anchor = 显式未绑定:目标虽命中 default,也不许推导
    const back = toCanvas(round)
    expect(back.edges.find((e) => e.id === 'e:verdict:revise')?.sourceHandle).toBeUndefined()
    expect(back.edges.find((e) => e.id === 'e:verdict:report')?.sourceHandle).toBe('case_0')
    // 空绑定记录经 canonical 序列化/再导入字节稳定
    const once = serializeDoc(round)
    const reimported = JSON.parse(once) as WorkflowDoc
    expect(serializeDoc(fromCanvas(toCanvas(reimported), reimported))).toBe(once)
  })

  it('canonical:无锚点文档不落 edges 键(字节面只多「有锚点」的文档)', () => {
    const doc = loadBase('minimal-llm')
    const out = serializeDoc(fromCanvas(toCanvas(doc), doc))
    expect((JSON.parse(out) as WorkflowDoc).metadata?.canvas?.edges).toBeUndefined()
  })

  it('往返:锚点绑定经导出/导入不变(canonical 稳定)', () => {
    const doc = loadBase('with-positions')
    const once = serializeDoc(fromCanvas(toCanvas(doc), doc))
    const reimported = JSON.parse(once) as WorkflowDoc
    expect(serializeDoc(fromCanvas(toCanvas(reimported), reimported))).toBe(once)
  })

  it('connectEdges:新对新增边(id 同源 e:from:to);同一对再连 = 改绑锚点,不翻倍', () => {
    const one = connectEdges([], { source: 'verdict', target: 'report', sourceHandle: 'case_0', targetHandle: null })
    expect(one).toEqual([
      { id: 'e:verdict:report', source: 'verdict', target: 'report', sourceHandle: 'case_0', targetHandle: undefined },
    ])
    // 同一对节点换个锚点再连:改绑既有边(顶层的重复边会被 duplicate_edge 拒)
    const two = connectEdges(one, { source: 'verdict', target: 'report', sourceHandle: 'default', targetHandle: null })
    expect(two).toHaveLength(1)
    expect(two[0].id).toBe('e:verdict:report')
    expect(two[0].sourceHandle).toBe('default')
    // 反方向是另一条边;原边表不被就地改写(纯函数)
    expect(connectEdges(two, { source: 'revise', target: 'report', sourceHandle: null, targetHandle: null })).toHaveLength(2)
    expect(one[0].sourceHandle).toBe('case_0')
  })

  it('reconcileSourceHandles:cases 收敛后清掉失效绑定,无变更时返回 null', () => {
    const cs = toCanvas(loadBase('all-node-types'))
    // 绑定都有效 = 不动(返回 null 让调用方跳过 setState)
    expect(reconcileSourceHandles(cs.nodes, cs.edges)).toBeNull()
    // 砍掉唯一一条 case(case_0 消失)→ 该边绑定被清(回退手柄),default 绑定不受影响
    const nodes = cs.nodes.map((n) => {
      if (n.id !== 'verdict' || n.data.spec.type !== 'branch') return n
      return { ...n, data: { spec: { ...n.data.spec, cases: [] } } }
    })
    const next = reconcileSourceHandles(nodes, cs.edges)
    expect(next?.find((e) => e.id === 'e:verdict:report')?.sourceHandle).toBeUndefined()
    expect(next?.find((e) => e.id === 'e:verdict:revise')?.sourceHandle).toBe('default')
    // 原边表不被就地改写(纯函数)
    expect(cs.edges.find((e) => e.id === 'e:verdict:report')?.sourceHandle).toBe('case_0')
  })
})
