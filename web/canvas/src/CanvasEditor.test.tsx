import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { CanvasEditor } from './CanvasEditor.tsx'
import { blankDoc } from './defaults.ts'
import type { WorkflowDoc } from './model/dsl.ts'
import { loadBase } from './model/testdata.ts'
import { PANEL_RIGHT_KEY } from './prefs.ts'
import { TID, tid } from './ui/testids.ts'

describe('CanvasEditor(画布 UI)', () => {
  it('打开工作流设置清掉节点选中环,再次点击同一节点切回其属性', async () => {
    localStorage.clear()
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    const card = screen.getByTestId(tid.nodeCard('verdict'))
    fireEvent.click(card)
    expect(await screen.findByTestId(TID.nodePanel)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '工作流设置' }))
    expect(screen.getByText('触发参数')).toBeInTheDocument()
    expect(screen.queryByTestId(TID.nodePanel)).not.toBeInTheDocument()
    await waitFor(() => expect(card.closest('.react-flow__node')).not.toHaveClass('selected'))
    fireEvent.click(card)
    expect(await screen.findByTestId(TID.nodePanel)).toBeInTheDocument()
    expect(screen.getByLabelText('条件去向 1')).toHaveTextContent('最终报告 (report)')
    expect(screen.getByRole('button', { name: '工作流设置' })).toHaveAttribute('aria-pressed', 'false')
  })

  it('属性下拉选择分支目标同步导出路由与连线,删除目标后清除去向', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await user.click(await screen.findByLabelText('默认去向'))
    await user.click(await screen.findByRole('option', { name: '最终报告 (report)' }))
    let doc = JSON.parse(screen.getByTestId(TID.editorJson).textContent ?? '{}') as WorkflowDoc
    expect(doc.nodes.find((n) => n.id === 'verdict')).toMatchObject({ default: 'report' })
    expect(doc.edges?.filter((e) => e.from === 'verdict')).toEqual([{ from: 'verdict', to: 'report' }])
    fireEvent.click(screen.getByTestId(tid.nodeCard('report')))
    await user.click(await screen.findByTestId(TID.nodeDelete))
    await user.click(screen.getByTestId(TID.deleteConfirmSubmit))
    doc = JSON.parse(screen.getByTestId(TID.editorJson).textContent ?? '{}') as WorkflowDoc
    expect(doc.nodes.find((n) => n.id === 'verdict')).toMatchObject({ cases: [expect.objectContaining({ next: '' })], default: '' })
    expect(doc.edges?.filter((e) => e.from === 'verdict')).toEqual([])
  })

  it('缩略图可显式收起/找回,未保存与审批节点文案明确', async () => {
    const user = userEvent.setup()
    const { container } = render(<CanvasEditor initialDoc={blankDoc()} dirty />)
    expect(container.querySelector('.react-flow__minimap')).not.toBeNull()
    await user.click(screen.getByRole('button', { name: '隐藏缩略图' }))
    expect(container.querySelector('.react-flow__minimap')).toBeNull()
    await user.click(screen.getByRole('button', { name: '显示缩略图' }))
    expect(container.querySelector('.react-flow__minimap')).not.toBeNull()
    expect(screen.getAllByText('未保存')).toHaveLength(1)
    expect(screen.queryByText('未建档')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '审批节点' })).toBeInTheDocument()
  })

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

  it('T21.4 ④ 可见入口:选中节点才出现「删除节点」,第一下只布防不落盘', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    // 未选中节点 = 没有可删的东西:入口不虚设。
    expect(screen.queryByTestId(TID.nodeDelete)).not.toBeInTheDocument()

    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await user.click(await screen.findByTestId(TID.nodeDelete))
    // 危险动作第二下才生效:确认层在场,节点、连线、DSL 都还没动。
    // (testid 与 role 双锚:e2e 按 id 查、读屏按 role+name 查,两边都不许漂。)
    expect(screen.getByTestId(TID.deleteConfirm)).toBeInTheDocument()
    expect(screen.getByRole('group', { name: '确认删除' })).toBeInTheDocument()
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()
    expect(screen.getByTestId(TID.editorJson).textContent).toContain('"verdict"')
  })

  it('T21.4 ④ 确认后:T21.4 ③ 的锚点绑定不留悬空记录(节点/出边/DSL 一起走)', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    // 基线:verdict 是 branch,两条出边各有显式锚点绑定。
    const before = JSON.parse(screen.getByTestId(TID.editorJson).textContent ?? '{}')
    expect(before.metadata.canvas.edges).toHaveLength(2)

    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await user.click(await screen.findByTestId(TID.nodeDelete))
    await user.click(screen.getByTestId(TID.deleteConfirmSubmit))

    expect(screen.queryByTestId(tid.nodeCard('verdict'))).not.toBeInTheDocument()
    const doc = JSON.parse(screen.getByTestId(TID.editorJson).textContent ?? '{}')
    expect(doc.nodes.map((n: { id: string }) => n.id)).not.toContain('verdict')
    expect(doc.edges.filter((e: { from: string }) => e.from === 'verdict')).toHaveLength(0)
    // ③ 的面:绑定表跟着边一起消失(不能留 from=verdict 的孤儿记录)。
    expect((doc.metadata?.canvas?.edges ?? []).filter((e: { from: string }) => e.from === 'verdict')).toHaveLength(0)
  })

  it('T21.4 ④ 增项:放弃 / Esc 收起确认层后焦点回入口按钮,节点还在', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))

    // ① 点「放弃」
    await user.click(await screen.findByTestId(TID.nodeDelete))
    await user.click(screen.getByRole('button', { name: '放弃' }))
    await waitFor(() => expect(screen.queryByRole('group', { name: '确认删除' })).not.toBeInTheDocument())
    expect(screen.getByTestId(TID.nodeDelete)).toHaveFocus()
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()

    // ② Esc 同契约
    await user.click(screen.getByTestId(TID.nodeDelete))
    fireEvent.keyDown(screen.getByRole('group', { name: '确认删除' }), { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('group', { name: '确认删除' })).not.toBeInTheDocument())
    expect(screen.getByTestId(TID.nodeDelete)).toHaveFocus()
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()
  })

  it('T21.4 ④ 键盘 Delete 走同一条漏斗:先布防,确认后才删', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await screen.findByTestId(TID.nodeDelete)

    fireEvent.keyDown(document, { key: 'Delete' })
    await waitFor(() => expect(screen.getByRole('group', { name: '确认删除' })).toBeInTheDocument())
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()

    await user.click(screen.getByTestId(TID.deleteConfirmSubmit))
    expect(screen.queryByTestId(tid.nodeCard('verdict'))).not.toBeInTheDocument()
  })

  it('T21.4 ④ Esc 不依赖焦点在层内(收尾审计 M2:点画布/收起面板后 Esc 仍要收)', async () => {
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await user.click(await screen.findByTestId(TID.nodeDelete))
    expect(screen.getByTestId(TID.deleteConfirm)).toHaveFocus()

    // 焦点被点到层外(点画布 → activeElement=body):Esc 仍须收起
    ;(document.activeElement as HTMLElement | null)?.blur()
    fireEvent.keyDown(document.body, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByTestId(TID.deleteConfirm)).not.toBeInTheDocument())
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()
  })

  it('T21.4 ④ 面板收起时键盘删除:焦点回落节点卡片(收尾审计 M3 变异盲区)', async () => {
    localStorage.clear()
    localStorage.setItem(PANEL_RIGHT_KEY, '1')
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    expect(screen.queryByLabelText('属性面板')).not.toBeInTheDocument()

    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    fireEvent.keyDown(document, { key: 'Delete' })
    await waitFor(() => expect(screen.getByTestId(TID.deleteConfirm)).toBeInTheDocument())
    fireEvent.keyDown(document.body, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByTestId(TID.deleteConfirm)).not.toBeInTheDocument())
    // 入口按钮不存在(面板收起)⇒ 落到节点卡片(React Flow 节点 tabindex=0),
    // 不是 body —— 键盘用户还在原来的节点上。
    expect(document.querySelector('.react-flow__node[data-id="verdict"]')).toHaveFocus()
    expect(screen.getByTestId(tid.nodeCard('verdict'))).toBeInTheDocument()
  })

  it('T21.4 ④ 放行旗标是一次性的:第二次删除仍要重新布防(收尾审计 M3 变异盲区)', async () => {
    localStorage.clear()
    const user = userEvent.setup()
    render(<CanvasEditor initialDoc={loadBase('with-positions')} />)
    // 第一次:确认删除 verdict(这条会把 allowDelete 置位并消费掉)
    fireEvent.click(screen.getByTestId(tid.nodeCard('verdict')))
    await user.click(await screen.findByTestId(TID.nodeDelete))
    await user.click(screen.getByTestId(TID.deleteConfirmSubmit))
    expect(screen.queryByTestId(tid.nodeCard('verdict'))).not.toBeInTheDocument()

    // 第二次:选另一个节点按 Delete —— 若旗标粘住就会直删;必须先布防。
    fireEvent.click(screen.getByTestId(tid.nodeCard('report')))
    fireEvent.keyDown(document, { key: 'Delete' })
    await waitFor(() => expect(screen.getByTestId(TID.deleteConfirm)).toBeInTheDocument())
    expect(screen.getByTestId(tid.nodeCard('report'))).toBeInTheDocument()
  })
})
