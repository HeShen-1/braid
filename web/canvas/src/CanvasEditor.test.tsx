import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { CanvasEditor } from './CanvasEditor.tsx'
import { blankDoc } from './defaults.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { loadBase } from './model/testdata.ts'
import { PANEL_RIGHT_KEY } from './prefs.ts'
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
    // branch:出口端口逐条列出(case + default),与卡片下沿的锚点手柄同集合
    expect(screen.getByText('#1 → report')).toBeInTheDocument()
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

  it('branch 多锚点(T21.4 ③):每个出口一个可连手柄,单出口节点不虚构锚点', () => {
    render(<CanvasEditor initialDoc={loadBase('all-node-types')} />)
    const verdict = screen.getByTestId(tid.nodeCard('verdict'))
    // 端口行与手柄同集合:case_0(→report)+ default(→revise)
    expect(verdict.querySelector('[data-port="case_0"]')).not.toBeNull()
    expect(verdict.querySelector('[data-anchor="case_0"]')).not.toBeNull()
    expect(verdict.querySelector('[data-anchor="default"]')).not.toBeNull()
    // 无 id 兜底手柄必须排在最前:React Flow 对 sourceHandle 缺省的边取 handles[0]
    // (@xyflow/system getHandle),排在锚点后面就等于把这类边挂到 case_0 上。
    const handles = verdict.querySelectorAll('[data-anchor]')
    expect(handles[0].getAttribute('data-anchor')).toBe('fallback')
    expect(handles[1].getAttribute('data-anchor')).toBe('case_0')
    // 单出口节点(approval)= 单缺省手柄,没有锚点属性
    expect(screen.getByTestId(tid.nodeCard('review')).querySelector('[data-anchor]')).toBeNull()
  })

  it('属性面板可收起(七条反馈 ②):开关在顶栏,收起后整列让给画布且留痕', async () => {
    localStorage.clear()
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} />)
    expect(screen.getByLabelText('属性面板')).toBeInTheDocument()
    expect(screen.getByLabelText('收起属性面板')).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByTestId(TID.panelRightToggle))
    expect(screen.queryByLabelText('属性面板')).not.toBeInTheDocument()
    // 收起后画布仍在,开关换成展开语义(不靠悬浮按钮找回)。
    expect(screen.getByLabelText('展开属性面板')).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByTestId(TID.editor)).toBeInTheDocument()
    // 折叠态是跨会话偏好,不是一次性状态。
    expect(localStorage.getItem(PANEL_RIGHT_KEY)).toBe('1')

    await user.click(screen.getByTestId(TID.panelRightToggle))
    expect(screen.getByLabelText('属性面板')).toBeInTheDocument()
  })

  it('面板收起时点「工作流设置」先把面板放回来(否则像没反应)', async () => {
    localStorage.clear()
    localStorage.setItem(PANEL_RIGHT_KEY, '1')
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('minimal-llm')} />)
    expect(screen.queryByLabelText('属性面板')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '工作流设置' }))
    expect(screen.getByLabelText('属性面板')).toBeInTheDocument()
    expect(screen.getByText('身份')).toBeInTheDocument()
  })
})
