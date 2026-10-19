// UX-F1-b:节点类型色是视觉契约,不随主题切换丢失可读性。
// 这里只核 CSS 中的最终不透明底/字色,不把截图像素当作唯一证据。
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const css = readFileSync(resolve(import.meta.dirname, 'styles/canvas-editor.css'), 'utf8')
const types = ['llm', 'tool', 'branch', 'approval', 'parallel'] as const

function blockAfter(marker: string): string {
  const start = css.indexOf(marker)
  if (start < 0) throw new Error(`missing CSS marker ${marker}`)
  const end = css.indexOf('\n}', start)
  if (end < 0) throw new Error(`unterminated CSS block ${marker}`)
  return css.slice(start, end + 2)
}

function value(block: string, name: string): string {
  const match = block.match(new RegExp(`--${name}\\s*:\\s*(#[0-9a-fA-F]{6})`))
  if (match === null) throw new Error(`missing ${name}`)
  return match[1]
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16) / 255)
  const linear = channels.map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4))
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2]
}

function contrast(background: string, foreground: string): number {
  const a = luminance(background)
  const b = luminance(foreground)
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
}

describe('UX-F1-b canvas type colors', () => {
  const dark = blockAfter('.editor {')
  const light = blockAfter(":root[data-theme='light'] .editor {")

  it.each([
    ['dark', dark],
    ['light', light],
  ] as const)('%s has readable type card text', (theme, block) => {
    for (const type of types) {
      const ratio = contrast(
        value(block, `canvas-type-${type}-bg`),
        value(block, `canvas-type-${type}-fg`),
      )
      expect(ratio, `${theme}/${type}`).toBeGreaterThanOrEqual(4.5)
    }
  })

  it('keeps five type colors in both theme blocks', () => {
    for (const type of types) {
      expect(value(dark, `canvas-type-${type}-bg`)).toMatch(/^#[0-9a-f]{6}$/i)
      expect(value(light, `canvas-type-${type}-bg`)).toMatch(/^#[0-9a-f]{6}$/i)
    }
  })

  it('scopes the canvas palette so read-only definition graphs keep neutral fallbacks', () => {
    for (const type of types) {
      expect(css).toMatch(new RegExp(`\\.editor \\.bn\\[data-type='${type}'\\]\\s*\\{\\s*--node-bg:`))
      expect(css).not.toMatch(new RegExp(`(?:^|\\n)\\.bn\\[data-type='${type}'\\]\\s*\\{\\s*--node-bg:`))
    }
  })
})
