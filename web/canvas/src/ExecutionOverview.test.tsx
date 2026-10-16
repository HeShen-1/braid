import { StrictMode } from 'react'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, type ExecutionSummary } from './api.ts'
import { ExecutionOverview } from './ExecutionOverview.tsx'
import { refreshConsole } from './refresh.ts'

const rows = [
  { created_at: '2026-10-16T01:00:00Z' },
  { created_at: '2026-10-16T03:00:00Z' },
] as ExecutionSummary[]
function response(total: number, items = rows) { return { items, page: { total, limit: 100, offset: 0 } } }
const refreshButton = () => screen.getByRole('button', { name: '刷新执行总览' })
const count = (label: string) => screen.getByLabelText(new RegExp(`^${label}：`))

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

describe('ExecutionOverview', () => {
  it('总数来自page.total，四次只读查询不携筛选或账号条件；图形明确样本和时间桶', async () => {
    const list = vi.spyOn(api, 'listExecutions').mockImplementation(async (query) => response(query?.status === 'RUNNING' ? 11 : query?.status === 'WAITING_APPROVAL' ? 7 : query?.status === 'FAILED' ? 3 : 1234))
    render(<ExecutionOverview onAuthExpired={() => {}} />)
    await waitFor(() => expect(refreshButton()).toHaveAttribute('aria-disabled', 'false'))
    expect(count('保留执行总数')).toHaveTextContent('1,234条')
    expect(count('执行中')).toHaveTextContent('11条')
    expect(count('等待审批')).toHaveTextContent('7条')
    expect(count('已失败')).toHaveTextContent('3条')
    expect(list.mock.calls.map(([query]) => query)).toEqual([
      { status: undefined, limit: 100, offset: 0 }, { status: 'RUNNING', limit: 1, offset: 0 },
      { status: 'WAITING_APPROVAL', limit: 1, offset: 0 }, { status: 'FAILED', limit: 1, offset: 0 },
    ])
    expect(screen.getByText(/当前租户 · 全部工作流/)).toBeInTheDocument()
    expect(screen.getByText(/已取 2 条 · 有效时间 2 条/)).toBeInTheDocument()
    expect(screen.getByRole('img')).toHaveAccessibleName(/每桶15分钟/)
    fireEvent.click(screen.getByText('查看各时间桶'))
    const table = screen.getByRole('table')
    expect(within(table).getByRole('caption')).toHaveTextContent('UTC，含起点、不含终点')
    expect(screen.getByText(/样本创建范围/)).toHaveTextContent('2026-10-16 01:00:00.000 至 2026-10-16 03:00:00.000')
    expect(screen.queryByText(/24小时|成功率|成本/)).not.toBeInTheDocument()
  })
  it('首帧未知、真正空数据为0，缺总数不把items.length冒充总数', async () => {
    let finish!: (value: ReturnType<typeof response>) => void
    vi.spyOn(api, 'listExecutions').mockImplementation(() => new Promise((resolve) => { finish = resolve }))
    const first = render(<ExecutionOverview onAuthExpired={() => {}} />)
    expect(count('保留执行总数')).toHaveTextContent('—')
    expect(screen.getByText('正在读取执行样本…')).toBeInTheDocument()
    first.unmount()
    vi.mocked(api.listExecutions).mockImplementation(async (query) => query?.status === undefined ? { items: rows, page: {} as ReturnType<typeof response>['page'] } : response(0, []))
    render(<ExecutionOverview onAuthExpired={() => {}} />)
    await waitFor(() => expect(refreshButton()).toHaveAttribute('aria-disabled', 'false'))
    expect(count('保留执行总数')).toHaveTextContent('—')
    expect(count('执行中')).toHaveTextContent('0条')
    expect(screen.getByText(/响应缺少有效总数/)).toBeInTheDocument()
    await act(async () => { finish(response(999)) })
    expect(count('已失败')).toHaveTextContent('0条')
  })
  it('失败保留最近成功值并标旧时间，其他卡仍更新；恢复后样本替换而不拼接', async () => {
    let failed = false
    const list = vi.spyOn(api, 'listExecutions').mockImplementation(async (query) => {
      if (failed && (query?.status === undefined || query.status === 'RUNNING')) throw new ApiError(403, 'role_denied', [])
      return response(failed ? 8 : 20)
    })
    render(<ExecutionOverview onAuthExpired={() => {}} />)
    await waitFor(() => expect(refreshButton()).toHaveAttribute('aria-disabled', 'false'))
    failed = true
    fireEvent.click(refreshButton())
    await screen.findByText(/创建分布读取失败/)
    expect(count('保留执行总数')).toHaveTextContent('20条')
    expect(count('执行中')).toHaveTextContent('20条')
    expect(count('等待审批')).toHaveTextContent('8条')
    expect(screen.getAllByText(/保留 .* UTC 的值/)).toHaveLength(2)
    expect(screen.getByText(/已取 2 条/)).toBeInTheDocument()
    list.mockImplementation(async () => response(0, []))
    fireEvent.click(refreshButton())
    await screen.findByText(/暂无执行样本/)
    expect(count('保留执行总数')).toHaveTextContent('0条')
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
    expect(screen.queryByText(/读取失败：/)).not.toBeInTheDocument()
  })
  it('缺失或重复时间诚实呈现，无有效跨度时不画图', async () => {
    vi.spyOn(api, 'listExecutions').mockResolvedValue(response(3, [rows[0], rows[0], { created_at: '' } as ExecutionSummary]))
    render(<ExecutionOverview onAuthExpired={() => {}} />)
    await screen.findByText(/缺失或无效时间 1 条，未计入分布/)
    expect(screen.getByText(/时间点不足，暂不绘制分布/)).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })
  it('首次读取失败保留未知和重试出口，401统一上抛一次', async () => {
    const expired = vi.fn()
    vi.spyOn(api, 'listExecutions').mockRejectedValue(new ApiError(401, 'unauthorized', []))
    render(<ExecutionOverview onAuthExpired={expired} />)
    await waitFor(() => expect(expired).toHaveBeenCalledTimes(1))
    expect(count('保留执行总数')).toHaveTextContent('—')
    expect(screen.getByText('样本未知，请刷新重试。')).toBeInTheDocument()
    expect(refreshButton()).toHaveAttribute('aria-disabled', 'false')
    expect(screen.queryByText(/暂无执行样本/)).not.toBeInTheDocument()
  })
  it('5秒刷新、操作事件、在途去重和卸载清理；不累积轮询历史', async () => {
    vi.useFakeTimers()
    const list = vi.spyOn(api, 'listExecutions').mockResolvedValue(response(2))
    const view = render(<ExecutionOverview onAuthExpired={() => {}} />)
    await act(async () => {})
    expect(list).toHaveBeenCalledTimes(4)
    await act(async () => { vi.advanceTimersByTime(5000) })
    expect(list).toHaveBeenCalledTimes(8)
    const resolvers: Array<(value: ReturnType<typeof response>) => void> = []
    list.mockImplementation(() => new Promise((resolve) => { resolvers.push(resolve) }))
    act(() => { refreshConsole() })
    expect(list).toHaveBeenCalledTimes(12)
    await act(async () => { vi.advanceTimersByTime(15_000); refreshConsole() })
    expect(list).toHaveBeenCalledTimes(12)
    await act(async () => { resolvers.forEach((resolve) => resolve(response(1, [rows[0]]))) })
    expect(screen.getByText(/已取 1 条/)).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
    view.unmount()
    await act(async () => { vi.advanceTimersByTime(10_000); refreshConsole() })
    expect(list).toHaveBeenCalledTimes(12)
  })
  it('Enter手动刷新自然保留焦点，忙态可聚焦但重复Enter和空格不增加请求', async () => {
    const user = userEvent.setup()
    const list = vi.spyOn(api, 'listExecutions').mockResolvedValue(response(2))
    render(<ExecutionOverview onAuthExpired={() => {}} />)
    await waitFor(() => expect(refreshButton()).toHaveAttribute('aria-disabled', 'false'))
    const resolvers: Array<(value: ReturnType<typeof response>) => void> = []
    list.mockImplementation(() => new Promise((resolve) => { resolvers.push(resolve) }))
    refreshButton().focus()
    await user.keyboard('{Enter}')
    expect(list).toHaveBeenCalledTimes(8)
    expect(refreshButton()).not.toBeDisabled()
    expect(refreshButton()).toHaveAttribute('aria-disabled', 'true')
    expect(refreshButton()).toHaveFocus()
    await user.keyboard('{Enter} ')
    expect(list).toHaveBeenCalledTimes(8)
    await act(async () => { resolvers.forEach((resolve) => resolve(response(3))) })
    expect(refreshButton()).toHaveAttribute('aria-disabled', 'false')
    expect(refreshButton()).toHaveFocus()
  })
  it('自动5秒刷新前后保留原焦点，用户转移到其他控件后不被抢回', async () => {
    vi.useFakeTimers()
    const list = vi.spyOn(api, 'listExecutions').mockResolvedValue(response(2))
    render(<><ExecutionOverview onAuthExpired={() => {}} /><input aria-label="其他控件" /></>)
    await act(async () => {})
    const resolvers: Array<(value: ReturnType<typeof response>) => void> = []
    list.mockImplementation(() => new Promise((resolve) => { resolvers.push(resolve) }))
    refreshButton().focus()
    await act(async () => { vi.advanceTimersByTime(5000) })
    expect(list).toHaveBeenCalledTimes(8)
    expect(refreshButton()).not.toBeDisabled()
    expect(refreshButton()).toHaveAttribute('aria-disabled', 'true')
    expect(refreshButton()).toHaveFocus()
    await act(async () => { resolvers.splice(0).forEach((resolve) => resolve(response(3))) })
    expect(refreshButton()).toHaveAttribute('aria-disabled', 'false')
    expect(refreshButton()).toHaveFocus()
    await act(async () => { vi.advanceTimersByTime(5000) })
    expect(list).toHaveBeenCalledTimes(12)
    const other = screen.getByRole('textbox', { name: '其他控件' })
    other.focus()
    await act(async () => { resolvers.splice(0).forEach((resolve) => resolve(response(4))) })
    expect(other).toHaveFocus()
    await act(async () => { vi.advanceTimersByTime(5000) })
    expect(list).toHaveBeenCalledTimes(16)
    expect(other).toHaveFocus()
    await act(async () => { resolvers.splice(0).forEach((resolve) => resolve(response(5))) })
    expect(other).toHaveFocus()
  })
  it('StrictMode旧请求迟到不覆盖新挂载结果或触发旧401', async () => {
    const expired = vi.fn()
    const rejectOld: Array<(error: unknown) => void> = []
    let calls = 0
    vi.spyOn(api, 'listExecutions').mockImplementation(() => ++calls <= 4
      ? new Promise((_, reject) => { rejectOld.push(reject) }) : Promise.resolve(response(3)))
    render(<StrictMode><ExecutionOverview onAuthExpired={expired} /></StrictMode>)
    await waitFor(() => expect(refreshButton()).toHaveAttribute('aria-disabled', 'false'))
    await act(async () => { rejectOld.forEach((reject) => reject(new ApiError(401, 'unauthorized', []))) })
    expect(count('保留执行总数')).toHaveTextContent('3条')
    expect(expired).not.toHaveBeenCalled()
  })
})
