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
