// ApprovalDetail 测试(T21.2 Lane A):元数据六项(旧 renderDetail 同口径)、
// 审批说明兜底文案与自由文本的文本节点纪律、决定时间线词表、非 WAITING 只读、
// 备注上限就近提示、提交期间双按钮禁用、驳回确认层。
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalDetail } from './ApprovalDetail.tsx'
import type { ApprovalDetail as ApprovalDetailData } from './api.ts'
import { TID } from './ui/testids.ts'

function detail(over: Partial<ApprovalDetailData> = {}): ApprovalDetailData {
  return {
    execution_id: '7',
    node_id: 'n1',
    node_status: 'WAITING_APPROVAL',
    deadline_at: '2999-01-01T00:00:00Z',
    approvers: ['alice', 'bob'],
    mode: 'any',
    instructions: '看两眼再批',
    pending_approvers: ['alice'],
    decisions: [],
    ...over,
  }
}

function renderDetail(over: Partial<Parameters<typeof ApprovalDetail>[0]> = {}) {
  const props = {
    detail: detail(),
    canDecide: true,
    loading: false,
    error: '',
    definitionName: 'wf',
    comment: '',
    commentError: '',
    busy: false,
    confirmReject: false,
    onCommentChange: vi.fn(),
    onRequestDecision: vi.fn(),
    onCancelReject: vi.fn(),
    onBack: vi.fn(),
    ...over,
  }
  render(<ApprovalDetail {...props} />)
  return props
}

describe('ApprovalDetail(审批详情)', () => {
  it('元数据六项:执行 ID / 节点 / 审批模式 / 审批人 / 待投名单 / 截止时间', () => {
    renderDetail()
    const meta = screen.getByTestId(TID.approvalMeta)
    expect(meta).toHaveTextContent('执行 ID')
    expect(meta).toHaveTextContent('7')
    expect(meta).toHaveTextContent('n1')
    expect(meta).toHaveTextContent('或签(任一票定局)')
    expect(meta).toHaveTextContent('alice, bob')
    expect(screen.getByTestId(TID.pendingApprovers)).toHaveTextContent('alice')
    expect(screen.getByTestId(TID.approvalDeadline)).toBeInTheDocument()
  })

  it('会签词面与待投名单空显示 —(旧 modeFace / renderDetail 同口径)', () => {
    renderDetail({ detail: detail({ mode: 'all', pending_approvers: [] }) })
    expect(screen.getByTestId(TID.approvalMeta)).toHaveTextContent('会签(全部批准才过,任一驳回即败)')
    expect(screen.getByTestId(TID.pendingApprovers)).toHaveTextContent('—')
  })

  it('审批说明为空 → 「(工作流未提供审批说明)」', () => {
    renderDetail({ detail: detail({ instructions: '' }) })
    expect(screen.getByTestId(TID.approvalInstructions)).toHaveTextContent('(工作流未提供审批说明)')
  })

  it('说明是服务端自由文本:走文本节点,尖括号原样显示不成标签', () => {
    const raw = '<img src=x onerror=alert(1)>审批说明'
    renderDetail({ detail: detail({ instructions: raw }) })
    const p = screen.getByTestId(TID.approvalInstructions)
    expect(p.textContent).toBe(raw)
    expect(p.innerHTML).toContain('&lt;img')
    expect(p.querySelector('img')).toBeNull()
  })

  it('决定时间线:decided_at · decided_by · 中文决定 + 可选备注', () => {
    renderDetail({
      detail: detail({
        decisions: [
          {
            id: 'd1',
            execution_id: '7',
            node_id: 'n1',
            round: 1,
            decision: 'APPROVED',
            decided_by: 'alice',
            comment: '同意',
            decided_at: '2026-10-09T02:00:00Z',
          },
          {
            id: 'd2',
            execution_id: '7',
            node_id: 'n1',
            round: 1,
            decision: 'REJECTED',
            decided_by: 'bob',
            comment: null,
            decided_at: '2026-10-09T03:00:00Z',
          },
        ],
      }),
    })
    const ul = screen.getByTestId(TID.approvalTimeline)
    expect(ul).toHaveTextContent('alice')
    expect(ul).toHaveTextContent('批准')
    expect(ul).toHaveTextContent('同意')
    expect(ul).toHaveTextContent('bob')
    expect(ul).toHaveTextContent('驳回')
    expect(ul.querySelectorAll('li')).toHaveLength(2)
  })

  it('WAITING_APPROVAL:决定表单在场(备注 + 批准 + 驳回)', () => {
    renderDetail()
    expect(screen.getByTestId(TID.approvalComment)).toBeInTheDocument()
    expect(screen.getByTestId(TID.approvalApprove)).toBeEnabled()
    expect(screen.getByTestId(TID.approvalReject)).toBeEnabled()
    expect(screen.queryByText('该审批已收口')).not.toBeInTheDocument()
  })

  it('收口节点只读:表单整块不在场,给收口说明(前置免无效提交)', () => {
    renderDetail({ detail: detail({ node_status: 'SUCCEEDED' }) })
    expect(screen.queryByTestId(TID.approvalComment)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalApprove)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.approvalReject)).not.toBeInTheDocument()
    expect(screen.getByText('该审批已收口')).toBeInTheDocument()
    expect(screen.getByText(/不再接受新的决定/)).toBeInTheDocument()
  })

  it('提交期间:两个按钮同时禁用 + 备注/确认按钮同批禁用(in-flight 的 UI 面)', () => {
    renderDetail({ busy: true, confirmReject: true })
    expect(screen.getByTestId(TID.approvalApprove)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalReject)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalComment)).toBeDisabled()
    expect(screen.getByRole('button', { name: '确认驳回' })).toBeDisabled()
  })

  it('驳回确认层:布防时才出现;确认/放弃各走各的回调', async () => {
    const user = userEvent.setup()
    const props = renderDetail({ confirmReject: true })
    const group = screen.getByRole('group', { name: '确认驳回' })
    expect(group).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '确认驳回' }))
    expect(props.onRequestDecision).toHaveBeenCalledWith('reject')

    await user.click(screen.getByRole('button', { name: '放弃' }))
    expect(props.onCancelReject).toHaveBeenCalledTimes(1)
  })

  it('T21.4 增项:布防即聚焦确认层,收起后焦点回「驳回」入口', () => {
    const props = {
      detail: detail(),
      canDecide: true,
    loading: false,
      error: '',
      comment: '',
      commentError: '',
      busy: false,
      confirmReject: true,
      onCommentChange: vi.fn(),
      onRequestDecision: vi.fn(),
      onCancelReject: vi.fn(),
      onBack: vi.fn(),
    }
    const { rerender } = render(<ApprovalDetail {...props} />)
    // 挂载即布防:焦点进浮起层(否则按 r 只产生视觉变化,读屏用户收不到通知)
    expect(screen.getByRole('group', { name: '确认驳回' })).toHaveFocus()
    // 布防态由组合根持有:它翻回 false 的那一帧,焦点回入口按钮
    rerender(<ApprovalDetail {...props} confirmReject={false} />)
    expect(screen.getByTestId(TID.approvalReject)).toHaveFocus()
  })

  it('未布防时不出现确认层,点按钮只发出一次请求(确认由组合根管)', async () => {
    const user = userEvent.setup()
    const props = renderDetail()
    expect(screen.queryByRole('group', { name: '确认驳回' })).not.toBeInTheDocument()
    await user.click(screen.getByTestId(TID.approvalReject))
    expect(props.onRequestDecision).toHaveBeenCalledWith('reject')
  })

  it('备注超上限:就近字段错误 + 两个提交按钮禁用', () => {
    renderDetail({ comment: 'x', commentError: '备注超过 2000 字上限。' })
    expect(screen.getByText('备注超过 2000 字上限。')).toBeInTheDocument()
    expect(screen.getByTestId(TID.approvalComment)).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByTestId(TID.approvalApprove)).toBeDisabled()
    expect(screen.getByTestId(TID.approvalReject)).toBeDisabled()
  })

  it('备注是受控输入(onCommentChange 上抛,组件不留内部态)', () => {
    const props = renderDetail()
    fireEvent.change(screen.getByTestId(TID.approvalComment), { target: { value: '补充说明' } })
    expect(props.onCommentChange).toHaveBeenCalledWith('补充说明')
    expect((screen.getByTestId(TID.approvalComment) as HTMLTextAreaElement).value).toBe('')
  })

  it('详情未就绪:加载中给加载文案;读失败给就近错误 + 返回队列出口', async () => {
    const user = userEvent.setup()
    renderDetail({ detail: null, loading: true })
    expect(screen.getByTestId(TID.approvalDetail)).toHaveTextContent('加载审批详情…')

    cleanup()
    const failed = renderDetail({ detail: null, loading: false, error: '你不是本节点的审批人。' })
    expect(screen.getByRole('alert')).toHaveTextContent('你不是本节点的审批人。')
    await user.click(screen.getByRole('button', { name: '返回队列' }))
    expect(failed.onBack).toHaveBeenCalledTimes(1)
  })

  it('标题:有定义名 → name · node;深链缺席 → 执行 id · node', () => {
    renderDetail({ definitionName: 'wf' })
    expect(screen.getByRole('heading', { name: 'wf · n1' })).toBeInTheDocument()

    cleanup()
    renderDetail({ definitionName: undefined })
    expect(screen.getByRole('heading', { name: '执行 7 · n1' })).toBeInTheDocument()
  })
})
