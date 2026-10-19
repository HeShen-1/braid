// 主题偏好(D-2026-10-09-console-v2 决策 1 + 6)。
//
// 三态:auto(跟随 prefers-color-scheme)/ light / dark;运营面仍以深色为默认,
// 工作流画布在无明确偏好时由路由提供浅色缺省;
// 载体 = localStorage(跨会话 UI 偏好),与认证 token 的 sessionStorage 分离
// (决策 6:两者不得互相引用作为放宽依据);落点 = `<html data-theme>`,
// 样式口径见 styles/index.css 的「默认世界」一节(auto 由 CSS 媒体查询解析)。
//
// index.html 里有一份**首绘前**运行的启动脚本(localStorage 是同步 API,
// 在样式表生效前就能定下属性,浅色用户不会看到一闪的深色)。两份必须同源:
// src/theme.test.ts 钉住 index.html 里的键与默认值与这里逐字一致。
import { useCallback, useEffect, useState } from 'react'

export type Theme = 'auto' | 'light' | 'dark'

export const THEME_KEY = 'braid.theme'
export const DEFAULT_THEME: Theme = 'dark'

const THEMES: readonly Theme[] = ['auto', 'light', 'dark']

export function isTheme(value: unknown): value is Theme {
  return typeof value === 'string' && (THEMES as readonly string[]).includes(value)
}

/** localStorage 可能被隐私模式/企业策略封掉:读失败退默认世界,不拦渲染。 */
export function readTheme(fallback: Theme = DEFAULT_THEME): Theme {
  const defaultTheme = isTheme(fallback) ? fallback : DEFAULT_THEME
  try {
    const raw = localStorage.getItem(THEME_KEY)
    return isTheme(raw) ? raw : defaultTheme
  } catch {
    return defaultTheme
  }
}

/** 是否已有用户明确选择；没有时由当前分面提供一次性默认值。 */
export function hasThemePreference(): boolean {
  try {
    return isTheme(localStorage.getItem(THEME_KEY))
  } catch {
    return false
  }
}

/** 写失败只丢偏好,不影响本次会话(当前值仍在内存与 DOM 上)。 */
export function writeTheme(theme: Theme): void {
  try {
    localStorage.setItem(THEME_KEY, theme)
  } catch {
    // 隐私模式:偏好不落盘,本次会话照常。
  }
}

/**
 * 落点只有这一个:data-theme 承载三态,token 切换全部由 CSS 承担 ——
 * 组件层不需要知道当前是哪套世界(决策 2:切主题 = 换一套别名值)。
 */
export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme
}

export function useTheme(defaultTheme: Theme = DEFAULT_THEME): { theme: Theme; setTheme: (theme: Theme) => void } {
  const [selected, setSelected] = useState<Theme>(() => readTheme(defaultTheme))
  const [explicit, setExplicit] = useState(() => hasThemePreference())
  const theme = explicit ? selected : defaultTheme

  // 首绘由 index.html 的启动脚本负责;这里兜住「组件挂载时 DOM 与状态不一致」
  // 的窗口(如测试环境直接 render,或启动脚本被策略拦掉)。
  useEffect(() => {
    applyTheme(theme)
  }, [theme])

  const setTheme = useCallback((next: Theme) => {
    applyTheme(next)
    writeTheme(next)
    setSelected(next)
    setExplicit(true)
  }, [])

  return { theme, setTheme }
}
