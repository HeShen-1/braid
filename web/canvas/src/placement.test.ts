import { describe, expect, it } from 'vitest'
import { defaultSpec } from './defaults.ts'
import type { FlowNode } from './model/convert.ts'
import type { NodeType } from './model/dsl.ts'
import { containsRect, newNodePosition, nodeSize, type Rect } from './placement.ts'

function node(type: NodeType, id: string): FlowNode {
  return { id, type, position: { x: 0, y: 0 }, data: { spec: defaultSpec(type, id) } }
}

function separated(a: Rect, b: Rect): boolean {
  return a.x + a.width <= b.x || b.x + b.width <= a.x || a.y + a.height <= b.y || b.y + b.height <= a.y
}

describe('新增节点落位', () => {
  it('高分支的实测边界旁连续加入四种节点,均留在可见区域且互不遮挡', () => {
    const visible: Rect = { x: 0, y: 0, width: 1200, height: 900 }
    const branch = { ...node('branch', 'tall'), measured: { width: 270, height: 600 } }
    const occupied: Rect[] = [{ x: 450, y: 100, ...nodeSize(branch) }]
    for (const type of ['llm', 'tool', 'branch', 'approval'] as const) {
      const size = nodeSize(node(type, type))
      const box = { ...newNodePosition(visible, size, occupied), ...size }
      expect(containsRect(visible, box)).toBe(true)
      for (const other of occupied) expect(separated(box, other)).toBe(true)
      occupied.push(box)
    }
    expect(occupied[0]).toEqual({ x: 450, y: 100, width: 270, height: 600 })
  })

  it('连续新增优先排在所选节点下方，即使第三个需要移动视口', () => {
    const visible = { x: 0, y: 0, width: 632, height: 672 }
    const size = { width: 200, height: 160 }
    const first = { ...newNodePosition(visible, size, []), ...size }
    const second = { ...newNodePosition(visible, size, [first], 24, first), ...size }
    const third = { ...newNodePosition(visible, size, [first, second], 24, second), ...size }
    expect(first.x).toBe((visible.width - size.width) / 2)
    expect(first.y).toBe((visible.height - size.height) / 2)
    expect([second.x, third.x]).toEqual([first.x, first.x])
    expect(second.y).toBe(first.y + first.height + 24)
    expect(third.y).toBe(second.y + second.height + 24)
    expect(separated(first, second)).toBe(true)
    expect(separated(second, third)).toBe(true)
    expect(containsRect(visible, third)).toBe(false)
  })

  it('下方有障碍时回退到其他可见空位，不覆盖任何节点', () => {
    const visible = { x: 0, y: 0, width: 900, height: 700 }
    const size = { width: 200, height: 160 }
    const anchor = { x: 350, y: 200, ...size }
    const obstacle = { x: 350, y: 384, ...size }
    const box = { ...newNodePosition(visible, size, [anchor, obstacle], 24, anchor), ...size }
    expect(containsRect(visible, box)).toBe(true)
    expect(separated(box, anchor)).toBe(true)
    expect(separated(box, obstacle)).toBe(true)
  })

  it('尚未测量的长分支按出口增长占位,字体缩放也进入尺寸估计', () => {
    const short = node('branch', 'short')
    const long = node('branch', 'long')
    if (long.data.spec.type !== 'branch') throw new Error('branch fixture')
    long.data.spec.cases = Array.from({ length: 30 }, () => ({ when: { left: '', op: 'eq', right: '' }, next: '' }))
    expect(nodeSize(long).height).toBeGreaterThan(nodeSize(short).height * 3)
    expect(nodeSize(long, 24).height).toBeGreaterThan(nodeSize(long).height)
    const measured = { ...long, measured: { width: 231, height: 739 } }
    expect(nodeSize(measured)).toEqual({ width: 231, height: 739 })
  })

  it('已平移的视口按流坐标寻找空位,不会回落到窗口原点', () => {
    const visible = { x: -1500, y: 2400, width: 800, height: 600 }
    const size = { width: 280, height: 160 }
    const position = newNodePosition(visible, size, [])
    expect(containsRect(visible, { ...position, ...size })).toBe(true)
    expect(position.x).toBeLessThan(-700)
    expect(position.y).toBeGreaterThan(2400)
  })

  it('视口已满时仍找到无重叠空位,原有节点坐标不变', () => {
    const occupied = [{ x: 0, y: 0, width: 300, height: 200 }]
    const before = structuredClone(occupied)
    const size = { width: 280, height: 160 }
    const box = { ...newNodePosition(occupied[0], size, occupied), ...size }
    expect(separated(box, occupied[0])).toBe(true)
    expect(occupied).toEqual(before)
  })
})
