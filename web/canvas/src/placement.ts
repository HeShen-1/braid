// 新节点只寻找空位,不重排已有图。React Flow 的测量尺寸优先;
// 尚未测量的卡片按现有 CSS 上界保守占位,分支高度随出口数增长。
import type { FlowNode } from './model/convert.ts'
import type { Point } from './model/dsl.ts'

export interface Size { width: number; height: number }
export interface Rect extends Point, Size {}

type MeasuredNode = FlowNode & { measured?: Partial<Size>; width?: number; height?: number }

export function nodeSize(node: MeasuredNode, rem = 16): Size {
  const spec = node.data.spec
  const fallbackHeight = spec.type === 'branch' ? Math.max(10, 6 + (spec.cases.length + 1) * 1.3) * rem : 10 * rem
  const width = node.measured?.width ?? node.width
  const height = node.measured?.height ?? node.height
  return {
    width: width !== undefined && Number.isFinite(width) && width > 0 ? width : 17.5 * rem,
    height: height !== undefined && Number.isFinite(height) && height > 0 ? height : fallbackHeight,
  }
}

export function containsRect(outer: Rect, inner: Rect): boolean {
  return inner.x >= outer.x && inner.y >= outer.y &&
    inner.x + inner.width <= outer.x + outer.width && inner.y + inner.height <= outer.y + outer.height
}

function overlaps(a: Rect, b: Rect, gap: number): boolean {
  return a.x < b.x + b.width + gap && a.x + a.width + gap > b.x &&
    a.y < b.y + b.height + gap && a.y + a.height + gap > b.y
}

/** 优先可见区域内离中心最近的空位;拥挤时返回最近的区域外空位供视口显露。 */
export function newNodePosition(visible: Rect, size: Size, occupied: Rect[], gap = 24): Point {
  const center = { x: visible.x + (visible.width - size.width) / 2, y: visible.y + (visible.height - size.height) / 2 }
  // 候选落在视口/障碍物的边界上,避免固定网格漏掉足够容纳一张卡的空隙。
  const xs = [...new Set([center.x, visible.x, visible.x + visible.width - size.width,
    ...occupied.flatMap((r) => [r.x - size.width - gap, r.x + r.width + gap])])]
  const ys = [...new Set([center.y, visible.y, visible.y + visible.height - size.height,
    ...occupied.flatMap((r) => [r.y - size.height - gap, r.y + r.height + gap])])]
  const candidates = xs.flatMap((x) => ys.map((y) => ({ x, y })))
  const distance = (p: Point) => (p.x - center.x) ** 2 + (p.y - center.y) ** 2
  candidates.sort((a, b) => Number(containsRect(visible, { ...b, ...size })) - Number(containsRect(visible, { ...a, ...size })) ||
    distance(a) - distance(b) || a.y - b.y || a.x - b.x)
  return candidates.find((p) => occupied.every((r) => !overlaps({ ...p, ...size }, r, gap))) ?? center
}
