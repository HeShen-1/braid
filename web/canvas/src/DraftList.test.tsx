// 左栏折叠(T21.3 七条反馈 ②):收起后留窄栏 + 展开键,折叠态进 localStorage。
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { DraftSummary } from './api.ts'
import { DraftList } from './DraftList.tsx'
import { PANEL_LEFT_KEY } from './prefs.ts'
import { TID, tid } from './ui/testids.ts'

const drafts: DraftSummary[] = [
  { id: '1', name: '解析工作流', version: 3, status: 'DRAFT', description: '', updated_at: '2026-10-01T02:00:00Z' },
]

function mount() {
  const onOpen = vi.fn()
  const onNew = vi.fn()
  render(<DraftList drafts={drafts} activeId="1" onOpen={onOpen} onNew={onNew} />)
  return { onOpen, onNew }
}

beforeEach(() => {
  localStorage.clear()
})

describe('DraftList(可收起左栏)', () => {
  it('加载与失败不是空态，重试保留已有列表并有明确出口', async () => {
    const onRetry = vi.fn()
    const props = { drafts: [], activeId: null, onOpen: vi.fn(), onNew: vi.fn(), onRetry }
    const { rerender } = render(<DraftList {...props} loading />)
    expect(screen.getByRole('status')).toHaveTextContent('正在加载工作流列表')
    expect(screen.queryByTestId(TID.draftEmpty)).not.toBeInTheDocument()
    rerender(<DraftList {...props} error="无法连接服务" />)
    expect(screen.getByRole('alert')).toHaveTextContent('工作流列表读取失败')
    expect(screen.queryByTestId(TID.draftEmpty)).not.toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('button', { name: '重试工作流列表' }))
    expect(onRetry).toHaveBeenCalledTimes(1)
    rerender(<DraftList {...props} drafts={drafts} loading />)
    expect(screen.getByTestId(tid.draftItem('1'))).toBeInTheDocument()
  })
  it('默认展开:列表与动作在场,收起键可达', () => {
    mount()
    expect(screen.getByTestId(TID.draftList)).toHaveAttribute('aria-label', '工作流列表')
    expect(screen.getByTestId(tid.draftItem('1'))).toBeInTheDocument()
    expect(screen.getByTestId(TID.draftNew)).toBeInTheDocument()
    expect(screen.getByTestId(TID.panelLeftToggle)).toHaveAccessibleName('收起工作流列表')
  })

  it('点收起:列表让位给窄栏,展开键留在原地,偏好落盘', async () => {
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByTestId(TID.panelLeftToggle))
    expect(screen.queryByTestId(tid.draftItem('1'))).not.toBeInTheDocument()
    expect(screen.getByTestId(TID.draftList)).toHaveClass('side-rail')
    expect(screen.getByTestId(TID.draftList)).not.toHaveAttribute('aria-label', '工作流列表')
    expect(screen.getByTestId(TID.panelLeftToggle)).toHaveAccessibleName('展开工作流列表')
    expect(localStorage.getItem(PANEL_LEFT_KEY)).toBe('1')
  })

  it('跨会话:上次收起 → 首屏就是收起的,点展开即回列表', async () => {
    localStorage.setItem(PANEL_LEFT_KEY, '1')
    const user = userEvent.setup()
    mount()
    expect(screen.queryByTestId(tid.draftItem('1'))).not.toBeInTheDocument()
    await user.click(screen.getByTestId(TID.panelLeftToggle))
    expect(screen.getByTestId(tid.draftItem('1'))).toBeInTheDocument()
    expect(localStorage.getItem(PANEL_LEFT_KEY)).toBe('0')
  })

  it('超 20 条全量可达:21 条草稿 21 行在场 + 计数 21(审计 F03 repro 反例)', () => {
    const many: DraftSummary[] = Array.from({ length: 21 }, (_, i) => ({
      id: String(i + 1),
      name: `wf-${i + 1}`,
      version: null,
      status: 'DRAFT',
      description: '',
      updated_at: '2026-10-04T05:00:00Z',
    }))
    render(<DraftList drafts={many} activeId={null} onOpen={vi.fn()} onNew={vi.fn()} />)
    for (let i = 1; i <= 21; i++) {
      expect(screen.getByTestId(tid.draftItem(String(i)))).toBeInTheDocument()
    }
    // 计数条此前停在"本页条数 20";数据面取全后这里即总数。
    expect(screen.getByText('21')).toBeInTheDocument()
  })
})
