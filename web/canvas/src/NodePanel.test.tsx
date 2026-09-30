import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { NodePanel } from './NodePanel.tsx'
import { WorkflowPanel } from './WorkflowPanel.tsx'
import type { DslNode, WorkflowDoc } from './model/dsl.ts'
import { loadBase } from './model/testdata.ts'

// Probe:受控回渲面板(update 直写 state),渲染期把最新值抛给闭包供断言。
let latestNode: DslNode | undefined
let latestDoc: WorkflowDoc | undefined

function NodeProbe({ initial }: { initial: DslNode }) {
  const [spec, setSpec] = useState(initial)
  latestNode = spec
  return <NodePanel spec={spec} update={(p) => setSpec((s) => p(s))} />
}

function DocProbe({ initial }: { initial: WorkflowDoc }) {
  const [doc, setDoc] = useState(initial)
  latestDoc = doc
  return <WorkflowPanel doc={doc} update={(p) => setDoc((d) => p(d))} nameLocked={false} />
}

function nodeFrom(name: string, id: string): DslNode {
  const doc = loadBase(name)
  const n = doc.nodes.find((x) => x.id === id)
  if (!n) throw new Error(`node ${id} not in fixture`)
  return n
}

describe('NodePanel', () => {
  it('公共字段:标题/描述/重试 immutable patch(原 spec 不被突变)', async () => {
    const user = userEvent.setup()
    const spec = nodeFrom('all-node-types', 'parse')
    render(<NodeProbe initial={spec} />)
    await user.clear(screen.getByLabelText('标题'))
    await user.type(screen.getByLabelText('标题'), '改名后的解析')
    expect(latestNode!.title).toBe('改名后的解析')
    expect(spec.title).toBe('文档解析') // 原 spec 未被突变
    await user.clear(screen.getByLabelText('重试次数'))
    await user.type(screen.getByLabelText('重试次数'), '5')
    expect(latestNode!.retry?.max_attempts).toBe(5)
  })

  it('llm:user_data 增行 + 模板编辑', async () => {
    const user = userEvent.setup()
    const spec = nodeFrom('all-node-types', 'parse')
    render(<NodeProbe initial={spec} />)
    await user.click(screen.getAllByRole('button', { name: '+ 添加' })[0]) // user_data 区
    const rows = screen.getAllByPlaceholderText('标签名')
    expect(rows).toHaveLength(2) // 原 1 行 + 新 1 行
    await user.type(screen.getByLabelText('提示词模板'), '!')
    if (latestNode!.type !== 'llm') throw new Error('unreachable')
    expect(latestNode!.prompt.template).toBe('解析下述文档,提取关键事实。!')
    expect(latestNode!.prompt.user_data).toHaveLength(2)
  })

  it('approval:approvers 逗号切分数组;mode=all 禁用留位(t14.2)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'review')} />)
    const approvers = screen.getByLabelText('审批人(逗号分隔)')
    await user.clear(approvers)
    await user.type(approvers, 'alice, bob, carol')
    if (latestNode!.type !== 'approval') throw new Error('unreachable')
    expect(latestNode!.approvers).toEqual(['alice', 'bob', 'carol'])
    // t14.2 已合入:all 会签可选(schema enum ["any","all"],D-t141 #5 消费)。
    const mode = screen.getByLabelText('签署模式') as HTMLSelectElement
    const all = mode.querySelector('option[value="all"]') as HTMLOptionElement
    expect(all.disabled).toBe(false)
    expect(mode.value).toBe('any')
  })

  it('branch:加分支行 + 默认去向', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'verdict')} />)
    await user.click(screen.getAllByRole('button', { name: '+ 添加' })[0])
    if (latestNode!.type !== 'branch') throw new Error('unreachable')
    expect(latestNode!.cases).toHaveLength(2)
    await user.clear(screen.getByLabelText('默认去向'))
    await user.type(screen.getByLabelText('默认去向'), 'report')
    expect(latestNode!.default).toBe('report')
  })

  it('parallel:body 类型切换 llm→tool 重建 body;foreach 编辑', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'fan')} />)
    await user.clear(screen.getByLabelText('foreach(数组引用)'))
    // userEvent 键盘语法:字面 { 双写转义({{ → {),} 非语法字符原样。
    await user.type(screen.getByLabelText('foreach(数组引用)'), '{{{{inputs.rows}}')
    if (latestNode!.type !== 'parallel') throw new Error('unreachable')
    expect(latestNode!.parallel.foreach).toBe('{{inputs.rows}}')
    await user.selectOptions(screen.getByLabelText('类型'), 'tool')
    if (latestNode!.type !== 'parallel') throw new Error('unreachable')
    expect(latestNode!.parallel.body.type).toBe('tool')
    expect(latestNode!.parallel.body).toHaveProperty('tool', '')
  })

  it('tool:入参值 JSON 嗅探(数字串→number,花括号→对象)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'read_doc')} />)
    const val = screen.getByLabelText('值 1')
    await user.clear(val)
    await user.type(val, '42')
    if (latestNode!.type !== 'tool') throw new Error('unreachable')
    expect(latestNode!.inputs?.path).toBe(42)
  })

  it('branch:右值字面量嗅探(数字恢复原生类型;引用保持字符串)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'verdict')} />)
    const right = screen.getByPlaceholderText('右值(exists 留空)')
    await user.clear(right)
    await user.type(right, '42')
    if (latestNode!.type !== 'branch') throw new Error('unreachable')
    expect(latestNode!.cases[0].when.right).toBe(42)
    await user.clear(right)
    await user.type(right, '{{{{nodes.x.output.y}}') // userEvent:字面 { 双写
    if (latestNode!.type !== 'branch') throw new Error('unreachable')
    expect(latestNode!.cases[0].when.right).toBe('{{nodes.x.output.y}}')
  })

  it('KVRows 撞键:改名到已有键被拒,两行数据均无损(对抗审查回归)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'review')} />)
    // review 节点 outputs = {decision, comment} 两行;逐键把行 1 打成
    // comment:前缀不撞即过、撞键字符被拒(行停在 commen)——断言点 =
    // 无任何行被静默吞掉(comment 行原值保全,总行数不减)。
    const key1 = screen.getByLabelText('输出名 1')
    await user.clear(key1)
    await user.type(key1, 'comment')
    if (latestNode!.type !== 'approval') throw new Error('unreachable')
    const out = latestNode!.outputs!
    expect(Object.keys(out)).toContain('comment')
    expect(Object.values(out).filter((v) => v === 'string')).toHaveLength(2)
  })

  it('op 词域:contains 可选(镜像 schema 8 值)', () => {
    render(<NodeProbe initial={nodeFrom('all-node-types', 'verdict')} />)
    const op = screen.getAllByRole('combobox').find((s) => (s as HTMLSelectElement).value === 'eq') as HTMLSelectElement
    expect([...op.options].map((o) => o.value)).toEqual(['eq', 'ne', 'gt', 'gte', 'lt', 'lte', 'contains', 'exists'])
  })
})

describe('WorkflowPanel', () => {
  it('name/description/inputs 编辑回写', async () => {
    const user = userEvent.setup()
    render(<DocProbe initial={loadBase('all-node-types')} />)
    await user.clear(screen.getByLabelText('名称'))
    await user.type(screen.getByLabelText('名称'), 'renamed-wf')
    expect(latestDoc!.name).toBe('renamed-wf')
    await user.click(screen.getAllByRole('button', { name: '+ 添加' })[0]) // inputs 区
    expect(latestDoc!.inputs).toHaveLength(3)
  })
})
