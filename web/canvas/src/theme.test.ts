// 主题三态(D-2026-10-09-console-v2 决策 1/6)的单元锚。
// CSS 侧的解析(light/auto/dark 三态落到哪套 token)是样式表的责任,
// 这里钉住:载体、默认世界、容错,以及「两份浅色档逐字节相同」这条结构约束。
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DEFAULT_THEME, THEME_KEY, applyTheme, isTheme, readTheme, writeTheme } from './theme.ts'

const css = readFileSync(resolve(import.meta.dirname, 'styles/index.css'), 'utf8')

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset.theme
})

describe('主题载体(决策 6:UI 偏好走 localStorage,与认证 token 分离)', () => {
  it('无偏好 = 深色:深色仍是默认世界', () => {
    expect(DEFAULT_THEME).toBe('dark')
    expect(readTheme()).toBe('dark')
  })

  it('三态各自存取;非法值退回默认世界', () => {
    for (const theme of ['auto', 'light', 'dark'] as const) {
      writeTheme(theme)
      expect(localStorage.getItem(THEME_KEY)).toBe(theme)
      expect(readTheme()).toBe(theme)
    }
    localStorage.setItem(THEME_KEY, 'sepia')
    expect(readTheme()).toBe('dark')
    expect(isTheme('sepia')).toBe(false)
    expect(isTheme(null)).toBe(false)
  })

  it('applyTheme 只落 data-theme —— 三态解析归 CSS,组件层不分支', () => {
    applyTheme('auto')
    expect(document.documentElement.dataset.theme).toBe('auto')
  })

  it('localStorage 被策略封掉:读退默认、写不抛(偏好不是故障)', () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    expect(readTheme()).toBe('dark')
    expect(() => writeTheme('light')).not.toThrow()
    get.mockRestore()
    set.mockRestore()
  })
})

describe('样式表结构锚(styles/index.css)', () => {
  // 只比声明行:先剥掉注释(第二份会写「理由见上」),值必须一字不差。
  const normalize = (block: string) =>
    block
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line !== '')
      .join('\n')

  it('浅色档的两份副本(light / auto+系统浅色)逐字节相同', () => {
    const blocks = [...css.matchAll(/\/\* light-world:start \*\/([\s\S]*?)\/\* light-world:end \*\//g)].map((m) =>
      normalize(m[1]),
    )
    // CSS 没有声明集复用机制 ⇒ 物理上必然两份;只改一份忘另一份必须红。
    expect(blocks).toHaveLength(2)
    expect(blocks[0]).toBe(blocks[1])
    expect(blocks[0]).toContain('--color-ground-0')
  })

  it('深色档只有一处定义(@theme):不再有 :root[data-theme=dark] 复制块', () => {
    expect(css).not.toMatch(/:root\[data-theme='dark'\]/)
    // 无属性 = 深色:浅色档只在 light / auto 下落地。
    expect(css).toContain(":root[data-theme='light'] {")
    expect(css).toContain("@media (prefers-color-scheme: light)")
  })

  it('浅色档声明的每个 token 都在 @theme(默认世界)有默认值', () => {
    // 血泪锚:primary-action / primary-ink / sig-fail-hi 曾只存在于「深色覆盖块」,
    // 默认世界改成 @theme 后它们无处落脚 —— 深色下主操作背景直接落空(透明)。
    const themeKeys = new Set(
      [...css.match(/@theme \{([\s\S]*?)\n\}/)![1].matchAll(/--color-([a-z0-9-]+):/g)].map((m) => m[1]),
    )
    const lightKeys = [
      ...css.match(/\/\* light-world:start \*\/([\s\S]*?)\/\* light-world:end \*\//)![1].matchAll(/--color-([a-z0-9-]+):/g),
    ].map((m) => m[1])
    expect(lightKeys.filter((k) => !themeKeys.has(k))).toEqual([])
  })
})

describe('启动脚本与 theme.ts 同源(public/assets/theme-boot.js)', () => {
  const boot = readFileSync(resolve(import.meta.dirname, '../public/assets/theme-boot.js'), 'utf8')
  const html = readFileSync(resolve(import.meta.dirname, '../index.html'), 'utf8')

  it('键与默认值逐字一致(改一处忘另一处必须红)', () => {
    expect(boot).toContain(`'${THEME_KEY}'`)
    expect(boot).toContain(`'${DEFAULT_THEME}'`)
    expect(boot).toContain('document.documentElement.dataset.theme')
  })

  it('index.html 引它,且不引入内联脚本(CSP 是 script-src self,内联在生产不执行)', () => {
    // 走 /assets/ 前缀:静态面只有资产前缀服务文件,其余路径是 SPA 入口。
    expect(html).toContain('<script src="/assets/theme-boot.js"></script>')
    // 内联 = 没有 src、且有脚本体的 <script>(注释里的说明不算)。
    const inline = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)].filter(
      (m) => m[1].trim() !== '',
    )
    expect(inline).toHaveLength(0)
  })
})
