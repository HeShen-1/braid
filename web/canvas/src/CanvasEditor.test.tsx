import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { CanvasEditor } from './CanvasEditor.tsx'
import { loadBase } from './model/testdata.ts'

describe('CanvasEditor(画布 UI)', () => {
  it('按 DSL 渲染节点与类型摘要(approval/parallel/tool/branch 全到场)', () => {
    render(<CanvasEditor initialDoc={loadBase('all-node-types')} />)
    // approval:审批人 + 或签/时长
    expect(screen.getByText('alice, bob')).toBeInTheDocument()
    expect(screen.getByText('或签 · 24h')).toBeInTheDocument()
    // parallel:foreach 引用剥壳 + body 类型
    expect(screen.getByText('foreach inputs.items')).toBeInTheDocument()
    expect(screen.getByText('body LLM · wait_all')).toBeInTheDocument()
    // tool:工具名
    expect(screen.getByText('file_read')).toBeInTheDocument()
    // branch:分支数 + 默认去向
    expect(screen.getByText('1 个分支')).toBeInTheDocument()
    expect(screen.getByText('默认 → revise')).toBeInTheDocument()
    // llm 摘要:model 缺省与模板首行
    expect(screen.getAllByText('默认模型').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByText('解析下述文档,提取关键事实。')).toBeInTheDocument()
  })

  it('palette 加 LLM 节点:新 spec 默认值 + 词域合规 id', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} />)
    await user.click(screen.getByTestId('add-llm'))
    // 标题缺省回落 id;minimal-llm 原节点 id 是 only,新 id = llm。
    expect(screen.getByText('llm', { selector: '.bn-title' })).toBeInTheDocument()
    expect(screen.getByText('待填提示词')).toBeInTheDocument()
    expect(screen.getAllByText('默认模型')).toHaveLength(2)
  })

  it('palette 加并行节点:基名 fan(词域合规)', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} />)
    await user.click(screen.getByTestId('add-parallel'))
    expect(screen.getByText('fan', { selector: '.bn-title' })).toBeInTheDocument()
    expect(screen.getByText('foreach 待填')).toBeInTheDocument()
  })

  it('DSL 预览反映导出产物(canonical 序)', () => {
    render(<CanvasEditor initialDoc={loadBase('all-node-types')} />)
    const pre = screen.getByLabelText('dsl-preview')
    expect(pre.textContent).toContain('"dsl": "1.0"')
    expect(pre.textContent).toContain('"name": "canvas-showcase"')
    expect(pre.textContent).toContain('"foreach": "{{inputs.items}}"')
  })
})
