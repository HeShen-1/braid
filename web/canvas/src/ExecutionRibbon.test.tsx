// ExecutionRibbon 测试(T21.1 Lane B):时间映射是纯函数(ribbonModel),
// 组件只负责画 —— 线性几何、终态冻结、now 推进、挂起状态锚、空态都钉死。
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExecutionRibbon, ribbonModel, type RibbonNode } from './ExecutionRibbon.tsx'
import { TID, tid } from './ui/testids.ts'
import { fmtDateTime } from './ui/time.tsx'

const T0 = Date.parse('2026-10-09T01:00:00Z')

function iso(ms: number): string {
  return new Date(ms).toISOString()
}

function node(over: Partial<RibbonNode> & { node_id: string; instance_no: number }): RibbonNode {
  return {
    type: 'llm',
    status: 'PENDING',
    attempts: 1,
    max_attempts: 1,
    error_code: null,
    error_message: null,
    started_at: null,
    finished_at: null,
    ...over,
  }
}

function draw(nodes: RibbonNode[], opts?: { startedAt?: string | null; finishedAt?: string | null; live?: boolean; selected?: string | null }) {
  const onSelectNode = vi.fn()
  render(
    <ExecutionRibbon
      nodes={nodes}
      startedAt={opts?.startedAt === undefined ? iso(T0) : opts.startedAt}
      finishedAt={opts?.finishedAt ?? null}
      live={opts?.live ?? true}
      selectedNodeId={opts?.selected ?? null}
      onSelectNode={onSelectNode}
    />,
  )
  return { onSelectNode }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('ribbonModel(时间映射纯函数)', () => {
  const nodes = [
    node({ node_id: 'a', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 10_000) }),
    node({ node_id: 'b', instance_no: 0, status: 'RUNNING', started_at: iso(T0 + 10_000) }),
    node({ node_id: 'c', instance_no: 0, status: 'PENDING' }),
  ]

  it('线性铺开:区间 [最早 started_at, now],段 = [started_at, finished_at ?? now]', () => {
    const m = ribbonModel(nodes, { startedAt: iso(T0), finishedAt: null, live: true, now: T0 + 20_000 })
    expect(m.from).toBe(T0)
    expect(m.to).toBe(T0 + 20_000)
    expect(m.durationMs).toBe(20_000)
    expect(m.rows[0].seg).toMatchObject({ left: 0, width: 50, ongoing: false })
    expect(m.rows[1].seg).toMatchObject({ left: 50, width: 50, ongoing: true })
    // PENDING 不画段(只留空轨)
    expect(m.rows[2].seg).toBeNull()
    // 短区间 3 刻度:起点 / 中点 / 终点
    expect(m.ticks.map((t) => t.left)).toEqual([0, 50, 100])
    expect(m.gridlines).toEqual([])
  })

  it('刻度密度自适应:中段 5 刻度 + 网格线;超长区间(标签带日期)回到 3 个', () => {
    const long = [
      node({ node_id: 'a', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 600_000) }),
    ]
    const m = ribbonModel(long, { startedAt: iso(T0), finishedAt: iso(T0 + 600_000), live: false, now: T0 + 900_000 })
    expect(m.ticks.map((t) => t.left)).toEqual([0, 25, 50, 75, 100])
    expect(m.gridlines).toEqual([25, 50, 75])

    const days = [node({ node_id: 'a', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 72 * 3600_000) })]
    const m2 = ribbonModel(days, { startedAt: iso(T0), finishedAt: iso(T0 + 72 * 3600_000), live: false, now: T0 + 72 * 3600_000 })
    expect(m2.ticks.map((t) => t.left)).toEqual([0, 50, 100])
    expect(m2.gridlines).toEqual([])
  })

  it('终态冻结:右端 = 最新 finished_at,now 再远也不拉长区间', () => {
    const m = ribbonModel(nodes, {
      startedAt: iso(T0),
      finishedAt: iso(T0 + 20_000),
      live: false,
      now: T0 + 86_400_000,
    })
    expect(m.to).toBe(T0 + 20_000)
    expect(m.durationMs).toBe(20_000)
    expect(m.nowLeft).toBeNull()
    expect(m.rows[0].seg?.width).toBe(50)
  })

  it('now 落在区间内部时给出 now 游标(服务端时钟略超前)', () => {
    const m = ribbonModel(nodes, { startedAt: iso(T0), finishedAt: iso(T0 + 20_000), live: true, now: T0 + 10_000 })
    expect(m.to).toBe(T0 + 20_000)
    expect(m.nowLeft).toBe(50)
  })

  it('一个节点都没开始:无坐标轴、全部空轨(不编时间)', () => {
    const m = ribbonModel([node({ node_id: 'a', instance_no: 0 })], {
      startedAt: null,
      finishedAt: null,
      live: true,
      now: T0,
    })
    expect(m.from).toBeNull()
    expect(m.ticks).toEqual([])
    expect(m.rows[0].seg).toBeNull()
  })

  it('挂起总时长只累计 WAITING_APPROVAL 段', () => {
    const m = ribbonModel(
      [
        node({ node_id: 'gate', instance_no: 0, type: 'approval', status: 'WAITING_APPROVAL', started_at: iso(T0) }),
        node({ node_id: 'a', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 10_000) }),
      ],
      { startedAt: iso(T0), finishedAt: null, live: true, now: T0 + 240_000 },
    )
    expect(m.waitMs).toBe(240_000)
  })
})

describe('ExecutionRibbon(渲染)', () => {
  it('挂起段:data-state 承载原始 ENUM(斜纹由 CSS 的 [data-state] 承担),行内可见中文标签', () => {
    draw(
      [
        node({
          node_id: 'gate1',
          instance_no: 0,
          type: 'approval',
          status: 'WAITING_APPROVAL',
          started_at: iso(T0),
          error_code: 'approval_rejected',
          error_message: '审批被驳回',
        }),
      ],
      { selected: 'gate1' },
    )
    const row = screen.getByTestId(tid.nodeRow('gate1', 0))
    expect(row).toHaveAttribute('data-state', 'WAITING_APPROVAL')
    expect(row).toHaveAttribute('data-selected', 'true')
    expect(row).toHaveTextContent('等待审批')
    const seg = within(row).getByRole('img')
    expect(seg).toHaveAttribute('data-state', 'WAITING_APPROVAL')
    expect(seg.getAttribute('title')).toContain(fmtDateTime(iso(T0)))
    expect(seg.getAttribute('title')).toContain('进行中')
    // 失败原因就近可见(不依赖颜色)
    expect(row).toHaveTextContent('approval_rejected')
    expect(row).toHaveTextContent('审批被驳回')
  })

  it('PENDING 行:无段、空轨,仍给状态芯片', () => {
    draw([node({ node_id: 'later', instance_no: 0, status: 'PENDING' })])
    const row = screen.getByTestId(tid.nodeRow('later', 0))
    expect(within(row).queryAllByRole('img')).toHaveLength(0)
    expect(row).toHaveTextContent('待执行')
  })

  it('挂起总时长进面板头(未占 worker)', () => {
    // now 必须钉住:等待时长 = now - started_at,真实时钟下不可断言
    vi.useFakeTimers()
    vi.setSystemTime(T0 + 240_000)
    draw([
      node({ node_id: 'gate', instance_no: 0, type: 'approval', status: 'WAITING_APPROVAL', started_at: iso(T0) }),
    ])
    const panel = screen.getByTestId(TID.execRibbon)
    expect(panel).toHaveTextContent('挂起')
    expect(panel).toHaveTextContent('4m00s')
    expect(panel).toHaveTextContent('未占 worker')
  })

  it('点击行 = 选回滚目标(标签列是按钮,尺寸不止于指针)', () => {
    const { onSelectNode } = draw([
      node({ node_id: 'gate', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 1000) }),
    ])
    fireEvent.click(screen.getByTestId(tid.nodeRow('gate', 0)))
    expect(onSelectNode).toHaveBeenCalledWith('gate')
  })

  it('进行中执行:now 每秒推进(右端 = now,已结束段随之被压缩)', () => {
    vi.useFakeTimers()
    vi.setSystemTime(T0 + 20_000)
    draw(
      [
        node({ node_id: 'a', instance_no: 0, status: 'SUCCEEDED', started_at: iso(T0), finished_at: iso(T0 + 5_000) }),
        node({ node_id: 'b', instance_no: 0, status: 'EXECUTING', started_at: iso(T0 + 5_000) }),
      ],
      { startedAt: iso(T0) },
    )
    const segA = () => within(screen.getByTestId(tid.nodeRow('a', 0))).getByRole('img')
    const segB = () => within(screen.getByTestId(tid.nodeRow('b', 0))).getByRole('img')
    expect(segA().style.width).toBe('25%')
    expect(segB().style.width).toBe('75%')
    act(() => {
      vi.advanceTimersByTime(20_000)
    })
    expect(segA().style.width).toBe('12.5%')
    expect(segB().style.width).toBe('87.5%')
  })

  it('空态:无节点记录时给空态而不是空轨道', () => {
    draw([], { startedAt: null, live: false })
    expect(screen.getByTestId(TID.execRibbon)).toHaveTextContent('暂无节点执行记录')
  })

  it('全部未开始:空轨 + 轴标「尚未开始」', () => {
    draw([node({ node_id: 'a', instance_no: 0 })], { startedAt: null })
    const panel = screen.getByTestId(TID.execRibbon)
    expect(panel).toHaveTextContent('尚未开始')
    expect(within(panel).queryAllByRole('img')).toHaveLength(0)
  })
})
