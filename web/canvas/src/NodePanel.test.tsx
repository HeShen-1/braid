import { render, screen } from '@testing-library/react'
import userEvent, { type UserEvent } from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { NodePanel } from './NodePanel.tsx'
import { WorkflowPanel } from './WorkflowPanel.tsx'
import type { DslNode, WorkflowDoc } from './model/dsl.ts'
import { loadBase } from './model/testdata.ts'
import { TID } from './ui/testids.ts'

/** 决策 3 收敛后 Select 不再是原生控件:两步交互 = 点触发器 → 点选项。 */
async function chooseOption(user: UserEvent, trigger: HTMLElement, option: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name: option }))
}

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
    await user.click(screen.getAllByRole('button', { name: '添加' })[0]) // user_data 区
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
    // Base UI select 的选项只在弹层挂载后存在 ⇒ 先点开再断言 all 可选。
    const mode = screen.getByLabelText('签署模式')
    expect(mode).toHaveTextContent('any · 或签')
    await user.click(mode)
    const all = await screen.findByRole('option', { name: /all · 会签/ })
    expect(all).not.toHaveAttribute('aria-disabled', 'true')
    await user.keyboard('{Escape}')
  })

  it('branch:加分支行 + 默认去向', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'verdict')} />)
    await user.click(screen.getAllByRole('button', { name: '添加' })[0])
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
    await chooseOption(user, screen.getByLabelText('类型'), '工具')
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

  it('op 词域:contains 可选(镜像 schema 8 值)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'verdict')} />)
    // op 的 label 即值本身(NodePanel:`OPS.map((o) => ({ value: o, label: o }))`),
    // 故弹层里的选项文本就是词域;Base UI 不把 value 落到 DOM 属性上。
    await user.click(screen.getByLabelText('条件运算符 1'))
    const labels = (await screen.findAllByRole('option')).map((o) => o.textContent)
    expect(labels).toEqual(['eq', 'ne', 'gt', 'gte', 'lt', 'lte', 'contains', 'exists'])
  })

  it('就近错误:审批人重复当场指出(schema approvers uniqueItems)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'review')} />)
    const approvers = screen.getByLabelText('审批人(逗号分隔)')
    await user.clear(approvers)
    await user.type(approvers, 'alice, alice')
    if (latestNode!.type !== 'approval') throw new Error('unreachable')
    expect(latestNode!.approvers).toEqual(['alice', 'alice']) // 值如实回写,面板只报警不静默改数
    expect(screen.getByRole('alert')).toHaveTextContent('重复的审批人：alice')
    expect(approvers).toHaveAttribute('aria-invalid', 'true')
  })

  it('就近错误:数值越界就地报(schema 1–10)', async () => {
    const user = userEvent.setup()
    render(<NodeProbe initial={nodeFrom('all-node-types', 'parse')} />)
    const retry = screen.getByLabelText('重试次数')
    await user.type(retry, '99')
    expect(screen.getByRole('alert')).toHaveTextContent('超出 schema 允许范围（1–10）')
    expect(retry).toHaveAttribute('aria-invalid', 'true')
  })

  it('T21.4 ④ 删除入口:传了 onDelete 才渲染,点击只发请求(确认层归画布管)', async () => {
    const user = userEvent.setup()
    const onDelete = vi.fn()
    render(<NodePanel spec={nodeFrom('all-node-types', 'parse')} update={() => {}} onDelete={onDelete} />)
    const entry = screen.getByTestId(TID.nodeDelete)
    expect(entry).toHaveAttribute('aria-label', '删除节点')
    await user.click(entry)
    expect(onDelete).toHaveBeenCalledTimes(1)
  })

  it('T21.4 ④ 未接删除面时不虚设入口(旧调用点零回归)', () => {
    render(<NodeProbe initial={nodeFrom('all-node-types', 'parse')} />)
    expect(screen.queryByTestId(TID.nodeDelete)).not.toBeInTheDocument()
  })
})

describe('WorkflowPanel', () => {
  it('name/description/inputs 编辑回写', async () => {
    const user = userEvent.setup()
    render(<DocProbe initial={loadBase('all-node-types')} />)
    await user.clear(screen.getByLabelText('名称'))
    await user.type(screen.getByLabelText('名称'), 'renamed-wf')
    expect(latestDoc!.name).toBe('renamed-wf')
    await user.click(screen.getAllByRole('button', { name: '添加' })[0]) // inputs 区
    expect(latestDoc!.inputs).toHaveLength(3)
  })
})
