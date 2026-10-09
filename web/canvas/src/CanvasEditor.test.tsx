import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { CanvasEditor } from './CanvasEditor.tsx'
import { blankDoc } from './defaults.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { loadBase } from './model/testdata.ts'
import { TID, tid } from './ui/testids.ts'

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
    await user.click(screen.getByTestId(tid.paletteBtn('llm')))
    // 标题缺省回落 id;minimal-llm 原节点 id 是 only,新 id = llm。
    expect(screen.getByText('llm', { selector: '.bn-title' })).toBeInTheDocument()
    expect(screen.getByText('待填提示词')).toBeInTheDocument()
    expect(screen.getAllByText('默认模型')).toHaveLength(2)
  })

  it('palette 加并行节点:基名 fan(词域合规)', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} />)
    await user.click(screen.getByTestId(tid.paletteBtn('parallel')))
    expect(screen.getByText('fan', { selector: '.bn-title' })).toBeInTheDocument()
    expect(screen.getByText('foreach 待填')).toBeInTheDocument()
  })

  it('DSL 预览抽屉反映导出产物(canonical 序)', () => {
    render(<CanvasEditor initialDoc={loadBase('all-node-types')} />)
    const pre = screen.getByTestId(TID.editorJson)
    expect(pre.textContent).toContain('"dsl": "1.0"')
    expect(pre.textContent).toContain('"name": "canvas-showcase"')
    expect(pre.textContent).toContain('"foreach": "{{inputs.items}}"')
  })

  it('节点卡片:类型进 data-type;编辑面不虚构执行状态;未配置走虚线', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('all-node-types')} />)
    const review = screen.getByTestId(tid.nodeCard('review'))
    expect(review).toHaveAttribute('data-type', 'approval')
    // 编辑面没有执行状态:不注入 status,也就没有状态色相。
    expect(review).not.toHaveAttribute('data-state')
    expect(review).not.toHaveAttribute('data-blank')
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toHaveAttribute('data-type', 'branch')
    // 新建节点 = 必填位还空着 → data-blank(虚线发丝线),依然不占状态色相。
    await user.click(screen.getByTestId(tid.paletteBtn('tool')))
    const tool = screen.getByTestId(tid.nodeCard('tool'))
    expect(tool).toHaveAttribute('data-blank', 'true')
    expect(tool).not.toHaveAttribute('data-state')
  })

  it('flash 顶栏状态文案;onDocChange 每次变更上报(挂载不算)', async () => {
    const user = userEvent.setup()
    const onDocChange = vi.fn<(doc: WorkflowDoc) => void>()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} flash="已保存" onDocChange={onDocChange} />)
    expect(screen.getByTestId(TID.editorFlash)).toHaveTextContent('已保存')
    // 挂载不是变更:壳层打开草稿时本就持有同一份文档。
    expect(onDocChange).not.toHaveBeenCalled()
    await user.click(screen.getByTestId(tid.paletteBtn('llm')))
    expect(onDocChange).toHaveBeenCalledTimes(1)
    expect(onDocChange.mock.calls[0][0].nodes.map((n) => n.id)).toContain('llm')
  })

  it('空画布与未选中节点各有空态(不是空白面板)', () => {
    render(<CanvasEditor initialDoc={blankDoc()} />)
    expect(screen.getByText('空白画布')).toBeInTheDocument()
    expect(screen.getByText('未选中节点')).toBeInTheDocument()
    expect(screen.getByTestId(TID.editor)).toBeInTheDocument()
  })
})
