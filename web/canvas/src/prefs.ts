// 跨会话 UI 偏好(localStorage)。
//
// 决策 6 的边界:D-t44 #1 管辖的是**认证 token**(Bearer + sessionStorage),
// 在它之外的是 UI 偏好 —— 主题(见 theme.ts)与工作流面两栏折叠态。
// 两者载体分离,不得互相引用作为放宽依据;这里只承载后者。
//
// 读写一律容错:隐私模式/企业策略会让 localStorage 抛异常,偏好丢了不算故障,
// 但绝不因此拦住渲染。
import { useCallback, useState } from 'react'

/** 左栏(工作流列表)是否收起。 */
export const PANEL_LEFT_KEY = 'braid.panel.left.collapsed'
/** 右栏(属性面板)是否收起。 */
export const PANEL_RIGHT_KEY = 'braid.panel.right.collapsed'

function readFlag(key: string, fallback: boolean): boolean {
  try {
    const raw = localStorage.getItem(key)
    if (raw === '1') return true
    if (raw === '0') return false
    return fallback
  } catch {
    return fallback
  }
}

function writeFlag(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? '1' : '0')
  } catch {
    // 偏好不落盘 ≠ 故障:本次会话的值仍在内存里。
  }
}

/** 折叠态钩子:默认都展开(缺省视野给内容,收起是用户的减法)。 */
export function useCollapsed(key: string): [boolean, (next: boolean) => void] {
  const [collapsed, setCollapsed] = useState<boolean>(() => readFlag(key, false))
  const set = useCallback(
    (next: boolean) => {
      writeFlag(key, next)
      setCollapsed(next)
    },
    [key],
  )
  return [collapsed, set]
}
