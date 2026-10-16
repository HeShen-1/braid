// ApprovalsView 测试(T21.2 Lane A)——组合根的四条硬账:
//   ① 列表请求序号守卫(陈旧响应丢弃)
//   ② 进新详情清空上一条未提交备注
//   ③ in-flight 防抖(提交期间双按钮禁用,双击/连按只落一笔 POST)
//   ④ 共享错误词表保留已知原因与未知原码
// 外加:决定成功的回队列 + offset 归零 + 成功反馈、键盘快捷键(输入框聚焦
// 时让位)、401 统一登出、备注上限前置拦下。fetch 全 stub 按 URL 路由。
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { ApprovalsView } from './ApprovalsView.tsx'
import { tokenStore, type ApprovalDecisionRow, type ApprovalDetail as ApprovalDetailData, type PendingApproval } from './api.ts'
import { TID, tid } from './ui/testids.ts'
import { refreshConsole } from './refresh.ts'
import type { ApprovalSelection } from './ApprovalQueue.tsx'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function err(code: string, status: number): Response {
  return new Response(JSON.stringify({ error: code }), { status, headers: { 'Content-Type': 'application/json' } })
}

/** issue #213:异步 stub 的响应落在 act 之外时,应用侧的 setState 已提交、但注册
 *  键盘监听的 effect 可能还没重新跑 —— 此刻发 keydown 会落到闭包里还是旧
 *  actionable/items/confirmReject 的那份监听器上,事件被整段忽略。这些键都幂等
 *  (光标封顶、布防不落库、已布防时再按 r/Esc 才推进),所以「重发到生效为止」
 *  不改语义,只是不再赌调度顺序。 */
function pressUntil(key: string, cond: () => void) {
  return waitFor(() => {
    fireEvent.keyDown(document.body, { key })
    cond()
  })
}

const pathOf = (url: string) => url.split('?')[0]
const queryOf = (url: string) => new URLSearchParams(url.split('?')[1] ?? '')

type Route = (url: string, init?: RequestInit) => Response | Promise<Response>
function stub(route: Route) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => route(String(input), init))
  vi.stubGlobal('fetch', fn)
  return fn
}

function pending(exec: string, node: string, name = 'wf', version = 1): PendingApproval {
  return {
    execution_id: exec,
    node_id: node,
    definition_name: name,
    definition_version: version,
    execution_status: 'WAITING_APPROVAL',
    deadline_at: '2999-01-01T00:00:00Z',
    requested_at: '2026-10-15T08:00:00Z',
  }
}

function page(items: PendingApproval[], offset = 0, total = items.length) {
  return { items, page: { limit: 20, offset, total } }
}

function detail(over: Partial<ApprovalDetailData> = {}): ApprovalDetailData {
  return {
    execution_id: '7',
    node_id: 'n1',
    node_status: 'WAITING_APPROVAL',
    deadline_at: '2999-01-01T00:00:00Z',
    approvers: ['alice'],
    mode: 'any',
    instructions: '看两眼再批',
    pending_approvers: ['alice'],
    decisions: [],
    ...over,
  }
}

function decisionRow(over: Partial<ApprovalDecisionRow> = {}): ApprovalDecisionRow {
  return {
    id: 'd1',
    execution_id: '7',
    node_id: 'n1',
    round: 1,
    // 读面回显走请求面词表(internal/api approval.go decisionFace)。
    decision: 'approve',
    decided_by: 'alice',
    comment: null,
    decided_at: '2026-10-09T02:00:00Z',
    ...over,
  }
}

/** 常规审批面的最小路由:列表 / 详情 n1(可换)/ 决定。 */
function approvalStub(opts: {
  items?: PendingApproval[]
  total?: number
  detail?: ApprovalDetailData
  onDecision?: (body: unknown) => Response | Promise<Response>
  listUrl?: (url: string) => void
}) {
  return stub((url, init) => {
    const p = pathOf(url)
    if (p === '/api/v1/approvals/pending') {
      opts.listUrl?.(url)
      return ok(page(opts.items ?? [pending('7', 'n1')], Number(queryOf(url).get('offset') ?? 0), opts.total))
    }
    if (p === '/api/v1/executions/7/approvals/n1') return ok(opts.detail ?? detail())
    if (p === '/api/v1/executions/7/approvals/n2') return ok(detail({ node_id: 'n2' }))
    if (p.endsWith('/decision')) {
      const body: unknown = JSON.parse(String(init?.body))
      return opts.onDecision !== undefined ? opts.onDecision(body) : ok(decisionRow())
    }
    return ok({})
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
  tokenStore.clear()
  vi.useRealTimers()
})

describe('ApprovalsView(审批台:队列 + 详情 + 键盘)', () => {
  it('空队列5秒自动发现新增待办；刷新事件不依赖手动按钮', async () => {
    vi.useFakeTimers()
    let available = false
    stub(() => ok(page(available ? [pending('7', 'n1')] : [])))
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await act(async () => {})
    expect(screen.getByText('当前没有待办审批')).toBeInTheDocument()
    available = true
    await act(async () => { await vi.advanceTimersByTimeAsync(5001) })
    expect(screen.getByTestId(tid.approvalRow('7', 'n1'))).toBeInTheDocument()
    available = false
    await act(async () => { refreshConsole() })
    expect(screen.getByText('当前没有待办审批')).toBeInTheDocument()
  })

  it('j/k使行焦点和滚动跟随；Esc和成功决定返回现有行，空队列仍有焦点出口', async () => {
    let items = [pending('7', 'n1'), pending('6', 'n2')]
    const scroll = vi.fn()
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll })
    stub((url) => {
      if (url.includes('/pending')) return ok(page(items))
      if (url.endsWith('/decision')) { items = []; return ok(decisionRow({ execution_id: '6', node_id: 'n2' })) }
      if (url.includes('/approvals/n2')) return ok(detail({ execution_id: '6', node_id: 'n2' }))
      return ok({ triggered_by: 'author', input: { text: '决定上下文' }, definition_name: 'wf', definition_version: 1 })
    })
    function Harness() {
      const [selected, setSelected] = useState<ApprovalSelection | null>(null)
      return <ApprovalsView canDecide username="alice" selected={selected} onSelect={setSelected} onAuthExpired={() => {}} />
    }
    const user = userEvent.setup()
    render(<Harness />)
    await screen.findByTestId(tid.approvalRow('7', 'n1'))
    await user.keyboard('j')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveFocus()
    expect(scroll).toHaveBeenCalledWith({ block: 'nearest' })
    await user.keyboard('{Enter}')
    await screen.findByTestId(TID.approvalApprove)
    expect(screen.getByText(/触发人/)).toHaveTextContent('author')
    expect(screen.getByRole('link', { name: '查看执行详情与审计' })).toHaveAttribute('href', '/executions/6')
    await user.keyboard('{Escape}')
    expect(await screen.findByTestId(tid.approvalRow('6', 'n2'))).toHaveFocus()
    await user.keyboard('{Enter}')
    await screen.findByTestId(TID.approvalApprove)
    await user.keyboard('a')
    expect(await screen.findByText('当前没有待办审批')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('complementary', { name: '待办审批队列' })).toHaveFocus())
    expect(screen.getByRole('button', { name: '查看刚才的决定' })).toBeInTheDocument()
    delete (HTMLElement.prototype as Partial<HTMLElement>).scrollIntoView
  })

  it.each([{ canDecide: false, username: 'alice' }, { canDecide: true, username: 'outsider' }])('角色或本轮名单不足时快捷键/按钮都不写 $username', async (capability) => {
    const fn = approvalStub({})
    render(<ApprovalsView {...capability} selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByText('当前审批只读')
    expect(screen.queryByTestId(TID.approvalApprove)).not.toBeInTheDocument()
    fireEvent.keyDown(document.body, { key: 'a' })
    fireEvent.keyDown(document.body, { key: 'r' })
    expect(fn.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
  })
  it('队列装载 + 选中态由 URL 驱动:点行把 ids 交给路由,选中即换详情级', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    approvalStub({ items: [pending('7', 'n1'), pending('6', 'n2', 'other', 2)], total: 2 })
    const { rerender } = render(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)

    // 列表级:队列在场,详情不在(⑥ 两级视图,无第三态占位)
    const row = await screen.findByTestId(tid.approvalRow('7', 'n1'))
    expect(screen.getByTestId(TID.approvalQueue)).toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalDetail)).not.toBeInTheDocument()
    await user.click(row)
    expect(onSelect).toHaveBeenCalledWith({ executionId: '7', nodeId: 'n1' })

    // 详情级:详情占满全页,队列退场(不是左右两列)
    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.approvalDetail)).toBeInTheDocument()
    // 详情响应没有 definition_name:标题从队列行补(旧 openDetail 同口径)
    expect(await screen.findByRole('heading', { name: 'wf · n1' })).toBeInTheDocument()
    // 节点状态以原始 ENUM 挂在详情根上(与时间带行 data-state 同语言)
    expect(screen.getByTestId(TID.approvalDetail)).toHaveAttribute('data-state', 'WAITING_APPROVAL')
    expect(screen.queryByTestId(TID.approvalQueue)).not.toBeInTheDocument()
  })

  it('⑥ 两级视图:返回队列收详情回列表;深链直达详情级后仍可返回', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    // 队列表里没有选中行(深链 + 列表已翻页的形态):标题回退执行 id
    approvalStub({ items: [pending('6', 'n2', 'other')], total: 1 })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />,
    )
    expect(await screen.findByRole('heading', { name: '执行 7 · n1' })).toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalQueue)).not.toBeInTheDocument()

    await user.click(screen.getAllByRole('button', { name: '返回队列' })[0])
    expect(onSelect).toHaveBeenCalledWith(null)

    rerender(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.approvalQueue)).toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalDetail)).not.toBeInTheDocument()
  })

  it('① 陈旧响应丢弃:后发先至的旧页响应不得覆盖新页', async () => {
    const user = userEvent.setup()
    const gates: Array<(r: Response) => void> = []
    stub(() => new Promise<Response>((res) => gates.push(res)))
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await waitFor(() => expect(gates).toHaveLength(1))
    await act(async () => gates[0](ok(page([pending('1', 'a')], 0, 40))))
    expect(await screen.findByTestId(tid.approvalRow('1', 'a'))).toBeInTheDocument()

    // 第 2 页(先发,慢)—— 它才是"陈旧"的那笔
    await user.click(screen.getByTestId(TID.approvalNext))
    await waitFor(() => expect(gates).toHaveLength(2))
    await user.click(screen.getByTestId(TID.approvalPrev))
    await waitFor(() => expect(gates).toHaveLength(3))

    // 后发先至:第 1 页(新)先回,渲染它
    await act(async () => gates[2](ok(page([pending('2', 'b')], 0, 40))))
    expect(await screen.findByTestId(tid.approvalRow('2', 'b'))).toBeInTheDocument()

    // 第 2 页的旧响应姗姗来迟:必须被丢弃,不能把新页顶掉
    await act(async () => gates[1](ok(page([pending('3', 'c')], 20, 40))))
    expect(screen.queryByTestId(tid.approvalRow('3', 'c'))).not.toBeInTheDocument()
    expect(screen.getByTestId(tid.approvalRow('2', 'b'))).toBeInTheDocument()
  })

  it('② 进入新详情必须清空上一条未提交备注,提交时带的是空备注', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const posts: unknown[] = []
    approvalStub({ items: [pending('7', 'n1'), pending('7', 'n2')], total: 2, onDecision: (b) => (posts.push(b), ok(decisionRow())) })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />,
    )
    const ta = await screen.findByTestId(TID.approvalComment)
    fireEvent.change(ta, { target: { value: '给 n1 的备注' } })
    expect((screen.getByTestId(TID.approvalComment) as HTMLTextAreaElement).value).toBe('给 n1 的备注')

    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n2' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByRole('heading', { name: 'wf · n2' })
    expect((screen.getByTestId(TID.approvalComment) as HTMLTextAreaElement).value).toBe('')

    // 真正的风险面:残留备注会作为下一节点的 comment 落库
    await user.click(screen.getByTestId(TID.approvalApprove))
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]).toEqual({ decision: 'approve', comment: '' })
  })

  it('③ in-flight 防抖:双击与键盘连按只落一笔 POST,期间两个按钮同时禁用', async () => {
    const user = userEvent.setup()
    let postCount = 0
    const gates: Array<(r: Response) => void> = []
    approvalStub({
      onDecision: () => {
        postCount += 1
        return new Promise<Response>((res) => gates.push(res))
      },
    })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />,
    )
    const approve = await screen.findByTestId(TID.approvalApprove)

    await user.click(approve)
    await waitFor(() => expect(postCount).toBe(1))
    expect(approve).toBeDisabled()
    expect(screen.getByTestId(TID.approvalReject)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalComment)).toBeDisabled()

    fireEvent.click(approve) // 双击的第二下
    fireEvent.keyDown(document.body, { key: 'a' }) // 键盘连按
    await act(async () => {
      await Promise.resolve()
    })
    expect(postCount).toBe(1)

    await act(async () => gates[0](ok(decisionRow())))
    // 成功即回队列(onSelect(null)):两级视图下反馈条在列表级,rerender 同步导航
    rerender(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await waitFor(() => expect(screen.getByTestId(TID.approvalFlash)).toBeInTheDocument())
  })

  it('③′ 提交失败后闸门释放:错误就近呈现、详情不清场、可重试', async () => {
    const user = userEvent.setup()
    let postCount = 0
    approvalStub({
      onDecision: () => {
        postCount += 1
        return err('not_approver', 403)
      },
    })
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    const approve = await screen.findByTestId(TID.approvalApprove)
    await user.click(approve)
    expect(await screen.findByRole('alert')).toHaveTextContent('你不是本节点的审批人。')
    expect(screen.getByTestId(TID.approvalDetail)).toBeInTheDocument()
    await waitFor(() => expect(approve).toBeEnabled())

    await user.click(approve)
    await waitFor(() => expect(postCount).toBe(2))
  })

  it('③″ 提交在飞时切走:不清掉新节点的备注、不把人弹回队列;失败归队列错误条', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const gates: Array<(r: Response) => void> = []
    approvalStub({
      items: [pending('7', 'n1'), pending('7', 'n2')],
      total: 2,
      onDecision: () => new Promise<Response>((res) => gates.push(res)),
    })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />,
    )
    await user.click(await screen.findByTestId(TID.approvalApprove))
    await waitFor(() => expect(gates).toHaveLength(1))

    // 提交还没回来,用户已经切到 n2 并开始写备注
    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n2' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByRole('heading', { name: 'wf · n2' })
    fireEvent.change(screen.getByTestId(TID.approvalComment), { target: { value: 'n2 的备注' } })

    // n1 的响应此时才回来:不能动 n2 的输入、不能把 URL 弹回队列;
    // 反馈条挂在队列面板 —— 两级视图下回列表级即见(主路径:在本条上
    // 提交成功会自动回列表,反馈不迟到)。
    await act(async () => gates[0](ok(decisionRow())))
    expect(onSelect).not.toHaveBeenCalledWith(null)
    expect((screen.getByTestId(TID.approvalComment) as HTMLTextAreaElement).value).toBe('n2 的备注')
    expect(screen.queryByTestId(TID.approvalFlash)).not.toBeInTheDocument()

    // 失败面同理:n2 的决定失败响应迟到(此时用户已切到 n1)——错误归队列
    // (它是 n2 的),不得误挂 n1 的详情;两级视图下队列退场,回列表级即见
    // (错误条与反馈条一起在队列面板里)。
    fireEvent.keyDown(document.body, { key: 'a' })
    await waitFor(() => expect(gates).toHaveLength(2))
    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByRole('heading', { name: 'wf · n1' })
    await act(async () => gates[1](err('not_approver', 403)))
    expect(within(screen.getByTestId(TID.approvalDetail)).queryByRole('alert')).toBeNull()
    rerender(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.approvalFlash)).toHaveTextContent('已记录:批准')
    expect(within(screen.getByTestId(TID.approvalQueue)).getByRole('alert')).toHaveTextContent('你不是本节点的审批人。')
  })

  it('⑤ 队列键盘:j/k 移动光标、Enter 打开光标行', async () => {
    const onSelect = vi.fn()
    approvalStub({ items: [pending('7', 'n1'), pending('7', 'n2')], total: 2 })
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    const row1 = await screen.findByTestId(tid.approvalRow('7', 'n1'))
    const row2 = screen.getByTestId(tid.approvalRow('7', 'n2'))
    expect(row1).toHaveAttribute('data-cursor', 'true')

    await pressUntil('j', () => expect(row2).toHaveAttribute('data-cursor', 'true'))
    expect(row1).toHaveAttribute('data-cursor', 'false')

    await pressUntil('k', () => expect(row1).toHaveAttribute('data-cursor', 'true'))

    await pressUntil('j', () => expect(row2).toHaveAttribute('data-cursor', 'true'))
    await pressUntil('Enter', () => expect(onSelect).toHaveBeenCalledWith({ executionId: '7', nodeId: 'n2' }))
  })

  it('⑤′ 输入框聚焦时 a/r/j/k 一律不生效(打字不是命令);焦点离开后 r 布防、再按确认', async () => {
    const user = userEvent.setup()
    const posts: unknown[] = []
    approvalStub({ onDecision: (b) => (posts.push(b), ok(decisionRow({ decision: 'reject', comment: 'arjk' }))) })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />,
    )
    const ta = await screen.findByTestId(TID.approvalComment)

    await user.type(ta, 'arjk')
    expect((ta as HTMLTextAreaElement).value).toBe('arjk')
    expect(posts).toHaveLength(0)
    expect(screen.queryByRole('group', { name: '确认驳回' })).not.toBeInTheDocument()

    fireEvent.keyDown(ta, { key: 'r' })
    expect(screen.queryByRole('group', { name: '确认驳回' })).not.toBeInTheDocument()
    expect(posts).toHaveLength(0)

    // 焦点不在可编辑控件里:'r' 只布防(不 POST),再按一次才提交 —— 走
    // 与按钮完全相同的路径,备注也一起带上
    fireEvent.keyDown(document.body, { key: 'r' })
    expect(screen.getByRole('group', { name: '确认驳回' })).toBeInTheDocument()
    expect(posts).toHaveLength(0)

    fireEvent.keyDown(document.body, { key: 'r' })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]).toEqual({ decision: 'reject', comment: 'arjk' })
    // 成功即回队列:反馈条在列表级(rerender 同步 onSelect(null) 的导航)
    rerender(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.approvalFlash)).toHaveTextContent('已记录:驳回 · arjk')
  })

  it('⑤‴ 长按 r 不得定局:auto-repeat 事件一律不生效(评审 fix 1,安全面)', async () => {
    const posts: unknown[] = []
    approvalStub({ onDecision: (b) => (posts.push(b), ok(decisionRow({ decision: 'reject' }))) })
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.approvalComment)

    // 首次 keydown 只布防
    await pressUntil('r', () => expect(screen.getByRole('group', { name: '确认驳回' })).toBeInTheDocument())
    expect(posts).toHaveLength(0)

    // 操作系统的 auto-repeat 事件必须被整段忽略 —— 否则长按 = 布防 + 确认
    // 一气呵成,而驳回落库不可改判
    fireEvent.keyDown(document.body, { key: 'r', repeat: true })
    fireEvent.keyDown(document.body, { key: 'r', repeat: true })
    fireEvent.keyDown(document.body, { key: 'a', repeat: true })
    await new Promise((r) => setTimeout(r, 0))
    expect(posts).toHaveLength(0)
    expect(screen.getByRole('group', { name: '确认驳回' })).toBeInTheDocument()

    // 松开后再按一次(非 repeat)才允许提交
    await pressUntil('r', () => expect(posts).toHaveLength(1))
    expect(posts[0]).toMatchObject({ decision: 'reject' })
  })

  it('⑤″ a 直达提交;Esc 先收确认层、无确认层时回队列', async () => {
    const onSelect = vi.fn()
    const posts: unknown[] = []
    approvalStub({ onDecision: (b) => (posts.push(b), ok(decisionRow())) })
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.approvalApprove)

    await pressUntil('r', () => expect(screen.getByRole('group', { name: '确认驳回' })).toBeInTheDocument())
    fireEvent.keyDown(document.body, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('group', { name: '确认驳回' })).not.toBeInTheDocument())
    expect(posts).toHaveLength(0)

    await pressUntil('Escape', () => expect(onSelect).toHaveBeenCalledWith(null))

    await pressUntil('a', () => expect(posts).toHaveLength(1))
    expect(posts[0]).toEqual({ decision: 'approve', comment: '' })
  })

  it('⑥ 共享错误词表:后端码 → 中文文案,未知码保留原码', async () => {
    const user = userEvent.setup()
    const cases: Array<[string, number, string]> = [
      ['not_approver', 403, '你不是本节点的审批人。'],
      ['already_decided', 409, '已有审批人投出决定（或签首票定局）。'],
      ['bad_comment', 400, '备注超过 2000 字上限。'],
      ['weird_code', 500, '请求失败（weird_code）。'],
    ]
    for (const [code, status, text] of cases) {
      cleanup()
      approvalStub({ onDecision: () => err(code, status) })
      render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
      await user.click(await screen.findByTestId(TID.approvalApprove))
      expect(await screen.findByRole('alert')).toHaveTextContent(text)
    }
  })

  it('备注上限按 rune 计(与后端 []rune 同口径):星平面字符不被数成 2', async () => {
    approvalStub({})
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    const ta = await screen.findByTestId(TID.approvalComment)
    fireEvent.change(ta, { target: { value: '😀'.repeat(2000) } })
    expect(screen.queryByText('备注超过 2000 字上限。')).not.toBeInTheDocument()
    expect(screen.getByTestId(TID.approvalApprove)).toBeEnabled()
    expect(screen.getByText('2000 / 2000 字')).toBeInTheDocument()

    fireEvent.change(ta, { target: { value: '😀'.repeat(2001) } })
    expect(screen.getByText('备注超过 2000 字上限。')).toBeInTheDocument()
    expect(screen.getByTestId(TID.approvalApprove)).toBeDisabled()
  })

  it('详情请求也有序号守卫:快速换行时迟到的旧详情不得顶掉新详情', async () => {
    const gates: Record<'n1' | 'n2', Array<(r: Response) => void>> = { n1: [], n2: [] }
    stub((url) => {
      const p = pathOf(url)
      if (p === '/api/v1/approvals/pending') return ok(page([pending('7', 'n1'), pending('7', 'n2')], 0, 2))
      if (p.endsWith('/approvals/n1')) return new Promise<Response>((res) => gates.n1.push(res))
      if (p.endsWith('/approvals/n2')) return new Promise<Response>((res) => gates.n2.push(res))
      return ok({})
    })
    const { rerender } = render(
      <ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />,
    )
    await waitFor(() => expect(gates.n1).toHaveLength(1))
    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n2' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    await waitFor(() => expect(gates.n2).toHaveLength(1))

    await act(async () => gates.n2[0](ok(detail({ node_id: 'n2', approvers: ['bob'] }))))
    expect(await screen.findByRole('heading', { name: 'wf · n2' })).toBeInTheDocument()

    await act(async () => gates.n1[0](ok(detail({ node_id: 'n1', approvers: ['alice'] }))))
    expect(screen.getByRole('heading', { name: 'wf · n2' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'wf · n1' })).not.toBeInTheDocument()
  })

  it('列表读失败且无数据:不冒充「没有待办」(诚实原则)', async () => {
    stub(() => err('http_500', 500))
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    const empty = await screen.findByTestId(TID.approvalEmpty)
    expect(empty).not.toHaveTextContent('当前没有待办审批')
    // 空态首帧就在场(items=[] + loading),错误条要等读失败落地 —— 取样点必须等它。
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('请求失败（http_500）。'))
  })

  it('决定成功:POST body 逐字 + 回队列 + offset 归零重拉 + 成功反馈', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const posts: unknown[] = []
    const listOffsets: string[] = []
    approvalStub({
      total: 40,
      listUrl: (url) => listOffsets.push(queryOf(url).get('offset') ?? ''),
      onDecision: (b) => (posts.push(b), ok(decisionRow({ comment: '看过了' }))),
    })
    const { rerender } = render(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.approvalRow('7', 'n1'))

    // 先翻到第 2 页:决定成功后必须回第一页,否则下一屏落在空页
    await user.click(screen.getByTestId(TID.approvalNext))
    await waitFor(() => expect(listOffsets.at(-1)).toBe('20'))

    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={() => {}} />)
    await screen.findByTestId(TID.approvalApprove)
    fireEvent.change(screen.getByTestId(TID.approvalComment), { target: { value: '看过了' } })
    await user.click(screen.getByTestId(TID.approvalApprove))

    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]).toEqual({ decision: 'approve', comment: '看过了' })
    expect(onSelect).toHaveBeenCalledWith(null)
    // 真实 app:URL 随 onSelect(null) 回列表级;这里 rerender 同步该导航。
    rerender(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={() => {}} />)
    expect(await screen.findByTestId(TID.approvalFlash)).toHaveTextContent('已记录:批准 · 看过了')
    await waitFor(() => expect(listOffsets.at(-1)).toBe('0'))
  })

  it('刷新按钮:offset 归零重拉(旧实现 refresh 语义)', async () => {
    const user = userEvent.setup()
    const listOffsets: string[] = []
    approvalStub({ total: 40, listUrl: (url) => listOffsets.push(queryOf(url).get('offset') ?? '') })
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={() => {}} />)
    await screen.findByTestId(tid.approvalRow('7', 'n1'))
    await user.click(screen.getByTestId(TID.approvalNext))
    await waitFor(() => expect(listOffsets.at(-1)).toBe('20'))
    await user.click(screen.getByTestId(TID.approvalRefresh))
    await waitFor(() => expect(listOffsets.at(-1)).toBe('0'))
  })

  it('非 WAITING_APPROVAL:决定表单只读(收口节点免无效提交)', async () => {
    approvalStub({ detail: detail({ node_status: 'SUCCEEDED' }) })
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    expect(await screen.findByText('该审批已收口')).toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalComment)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalApprove)).not.toBeInTheDocument()
  })

  it('备注超 2000 字:提交前就近提示,按钮禁用,键盘同样被拦(零 POST)', async () => {
    let postCount = 0
    approvalStub({
      onDecision: () => {
        postCount += 1
        return ok(decisionRow())
      },
    })
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={() => {}} onAuthExpired={() => {}} />)
    const ta = await screen.findByTestId(TID.approvalComment)
    fireEvent.change(ta, { target: { value: 'x'.repeat(2001) } })
    expect(screen.getByText('备注超过 2000 字上限。')).toBeInTheDocument()
    expect(screen.getByTestId(TID.approvalApprove)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalReject)).toBeDisabled()

    fireEvent.keyDown(document.body, { key: 'a' })
    await act(async () => {
      await Promise.resolve()
    })
    expect(postCount).toBe(0)
  })

  it('列表 401 → onAuthExpired(统一登出,不落中文错误)', async () => {
    const onAuthExpired = vi.fn()
    stub(() => err('unauthorized', 401))
    render(<ApprovalsView canDecide username="alice" selected={null} onSelect={() => {}} onAuthExpired={onAuthExpired} />)
    await waitFor(() => expect(onAuthExpired).toHaveBeenCalled())
  })

  it('详情 401 → onAuthExpired;详情读失败(非 401)给就近错误 + 返回队列出口', async () => {
    const onAuthExpired = vi.fn()
    stub((url) => {
      const p = pathOf(url)
      if (p === '/api/v1/approvals/pending') return ok(page([pending('7', 'n1')]))
      return err('unauthorized', 401)
    })
    const onSelect = vi.fn()
    const { rerender } = render(<ApprovalsView canDecide username="alice" selected={null} onSelect={onSelect} onAuthExpired={onAuthExpired} />)
    await screen.findByTestId(tid.approvalRow('7', 'n1'))
    rerender(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={onAuthExpired} />)
    await waitFor(() => expect(onAuthExpired).toHaveBeenCalled())

    cleanup()
    onAuthExpired.mockClear()
    stub((url) => {
      const p = pathOf(url)
      if (p === '/api/v1/approvals/pending') return ok(page([pending('7', 'n1')]))
      return err('not_approver', 403)
    })
    const user = userEvent.setup()
    render(<ApprovalsView canDecide username="alice" selected={{ executionId: '7', nodeId: 'n1' }} onSelect={onSelect} onAuthExpired={onAuthExpired} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('你不是本节点的审批人。')
    expect(onAuthExpired).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: '返回队列' }))
    expect(onSelect).toHaveBeenCalledWith(null)
  })
})
