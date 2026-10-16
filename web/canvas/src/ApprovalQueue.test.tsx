// ApprovalQueue 测试(T21.2 Lane A):列内容、整行/按钮/键盘三种打开方式、
// 空态、翻页禁用规则(旧实现的 offset/total 口径)、已逾期标记、错误与反馈行。
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalQueue, PAGE_LIMIT } from './ApprovalQueue.tsx'
import type { PendingApproval } from './api.ts'
import { TID, tid } from './ui/testids.ts'

function pending(
  exec: string,
  node: string,
  name = 'wf',
  version = 1,
  deadline = '2999-01-01T00:00:00Z',
  requestedAt = '2026-10-15T08:00:00Z',
): PendingApproval {
  return {
    execution_id: exec,
    node_id: node,
    definition_name: name,
    definition_version: version,
    execution_status: 'WAITING_APPROVAL',
    deadline_at: deadline,
    requested_at: requestedAt,
  }
}

function renderQueue(over: Partial<Parameters<typeof ApprovalQueue>[0]> = {}) {
  const handlers = { onOpen: vi.fn(), onRefresh: vi.fn(), onPrev: vi.fn(), onNext: vi.fn() }
  render(
    <ApprovalQueue
      items={[pending('7', 'n1'), pending('6', 'n2', 'other', 2)]}
      total={2}
      offset={0}
      loading={false}
      error=""
      flash=""
      selected={null}
      cursor={0}
      {...handlers}
      {...over}
    />,
  )
  return handlers
}

describe('ApprovalQueue(待办队列)', () => {
  it('列与旧审批台等价 + T21.4 增项:工作流 name vX / 执行 ID / 节点 / 已等待 / 截止时间 / 处理', () => {
    renderQueue()
    const row = screen.getByTestId(tid.approvalRow('7', 'n1'))
    expect(row).toHaveTextContent('wf')
    expect(row).toHaveTextContent('v1')
    expect(row).toHaveTextContent('7')
    expect(row).toHaveTextContent('n1')
    expect(row).toHaveTextContent('处理')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveTextContent('other')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveTextContent('v2')
    // 表头带「已等待」列(增项可见形态)
    const headers = [...screen.getAllByRole('columnheader')].map((h) => h.textContent)
    expect(headers).toEqual(['工作流', '执行 ID', '节点', '已等待', '截止时间', '处理'])
  })

  it('已等待 = requested_at → now(挂起起点起的时长,挂标题给绝对时刻)', () => {
    // now - 90s ⇒ fmtDuration = "1m30s";用相对 now 的 requested_at 钉语义。
    const requested = new Date(Date.now() - 90_000).toISOString()
    renderQueue({ items: [pending('7', 'n1', 'wf', 1, '2999-01-01T00:00:00Z', requested)] })
    const cell = screen.getByTestId(tid.approvalWaited('7', 'n1'))
    expect(cell.textContent).toMatch(/^1m\d{2}s$/)
    // 悬停两面:绝对起点进 title(Duration 契约)
    expect(cell.querySelector('span')).toHaveAttribute('title', expect.stringContaining('→ 进行中'))
  })

  it('整行可点:点击行、点击「处理」、行上 Enter 都只调一次 onOpen', async () => {
    const user = userEvent.setup()
    const props = renderQueue()
    const row = screen.getByTestId(tid.approvalRow('7', 'n1'))

    await user.click(row)
    expect(props.onOpen).toHaveBeenCalledWith({ executionId: '7', nodeId: 'n1' })

    props.onOpen.mockClear()
    await user.click(screen.getAllByRole('button', { name: '处理' })[0])
    expect(props.onOpen).toHaveBeenCalledTimes(1)
    expect(props.onOpen).toHaveBeenCalledWith({ executionId: '7', nodeId: 'n1' })

    props.onOpen.mockClear()
    fireEvent.keyDown(row, { key: 'Enter' })
    expect(props.onOpen).toHaveBeenCalledTimes(1)
    fireEvent.keyDown(row, { key: ' ' })
    expect(props.onOpen).toHaveBeenCalledTimes(2)
  })

  it('选中与光标是两件事:data-selected 由 URL 选中态给,data-cursor 由 j/k 给', () => {
    renderQueue({ selected: { executionId: '6', nodeId: 'n2' }, cursor: 0 })
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveAttribute('data-selected', 'true')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveRole('row')
    expect(screen.getByTestId(tid.approvalRow('6', 'n2'))).toHaveAttribute('data-cursor', 'false')
    expect(screen.getByTestId(tid.approvalRow('7', 'n1'))).toHaveAttribute('data-selected', 'false')
    expect(screen.getByTestId(tid.approvalRow('7', 'n1'))).toHaveAttribute('data-cursor', 'true')
  })

  it('空态用专属锚:当前没有待办审批(加载中不冒充空)', () => {
    renderQueue({ items: [], total: 0 })
    expect(screen.getByTestId(TID.approvalEmpty)).toHaveTextContent('当前没有待办审批')
  })

  it('读失败且无数据:不冒充「没有待办」(诚实原则),错误条就近', () => {
    renderQueue({ items: [], total: 0, error: '请求失败(500)。' })
    expect(screen.getByRole('alert')).toHaveTextContent('请求失败(500)。')
    expect(screen.getByTestId(TID.approvalEmpty)).not.toHaveTextContent('当前没有待办审批')
    expect(screen.getByTestId(TID.approvalEmpty)).toHaveTextContent('待办队列未能读取')
  })

  it('加载中:aria-busy + 刷新按钮禁用(不闪空态)', () => {
    renderQueue({ items: [], total: 0, loading: true })
    expect(screen.getByTestId(TID.approvalQueue)).toHaveAttribute('aria-busy', 'true')
    expect(screen.getByTestId(TID.approvalRefresh)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalEmpty)).not.toHaveTextContent('当前没有待办审批')
  })

  it('翻页禁用规则:offset>0 才有上一页;offset+PAGE_LIMIT>=total 没有下一页', () => {
    renderQueue({ offset: 0, total: 40 })
    expect(screen.getByTestId(TID.approvalPrev)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalNext)).toBeEnabled()

    cleanup()
    renderQueue({ offset: 20, total: 40 })
    expect(screen.getByTestId(TID.approvalPrev)).toBeEnabled()
    expect(screen.getByTestId(TID.approvalNext)).toBeDisabled()

    cleanup()
    renderQueue({ items: [pending('7', 'n1')], offset: 0, total: 1 })
    expect(screen.getByTestId(TID.approvalNext)).toBeDisabled()
  })

  it('翻页按钮把动作交给调用方(组件不自己改 offset)', async () => {
    const user = userEvent.setup()
    const props = renderQueue({ offset: 20, total: 40 })
    await user.click(screen.getByTestId(TID.approvalPrev))
    await user.click(screen.getByTestId(TID.approvalNext))
    expect(props.onPrev).toHaveBeenCalledTimes(1)
    // total=40,offset=20 → 20+20>=40 无下一页(与旧实现同口径)
    expect(props.onNext).not.toHaveBeenCalled()
  })

  it('已逾期:data-overdue 标记 + 文字,不只靠色相', () => {
    renderQueue({ items: [pending('7', 'n1', 'wf', 1, '2020-01-01T00:00:00Z')] })
    const row = screen.getByTestId(tid.approvalRow('7', 'n1'))
    expect(row.querySelector('[data-overdue="true"]')).not.toBeNull()
    expect(row).toHaveTextContent('已逾期')
  })

  it('错误与成功反馈各自就近呈现(错误 role=alert,反馈 role=status)', () => {
    renderQueue({ error: '该审批已收口,无需再操作。', flash: '已记录:批准' })
    expect(screen.getByRole('alert')).toHaveTextContent('该审批已收口,无需再操作。')
    expect(screen.getByTestId(TID.approvalFlash)).toHaveTextContent('已记录:批准')
  })
})

describe('PAGE_LIMIT(与旧实现同值)', () => {
  it('= 20', () => {
    expect(PAGE_LIMIT).toBe(20)
  })
})
