// XSS 纪律锚(源码面,同审批台 textContent 契约):画布代码禁
// innerHTML 家族与 dangerouslySetInnerHTML——他人自由文本(标题/描述/
// 模板/审批说明)一律走 React 文本节点自动转义。React 运行时内部对
// innerHTML 的合法使用在 bundle 层,不在此锚面内。
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const BANNED = [
  'dangerouslySetInnerHTML',
  '.innerHTML',
  '.outerHTML',
  'insertAdjacentHTML',
  'document.write',
  'srcdoc',
]

function walk(dir: string, out: string[] = []): string[] {
  // 递归:src/model/ 与 src/nodes/ 是核心渲染/转换面,不递归 = 盲区
  // (对抗审查 MEDIUM——NodeViews 恰是自由文本渲染组件)。
  for (const name of readdirSync(dir)) {
    const full = resolve(dir, name)
    if (statSync(full).isDirectory()) {
      walk(full, out)
      continue
    }
    if (/\.(ts|tsx)$/.test(name) && !name.endsWith('.test.ts') && !name.endsWith('.test.tsx')) {
      out.push(full)
    }
  }
  return out
}

describe('XSS 纪律(源码扫描)', () => {
  it('src 全树(含 model/ nodes/)禁 innerHTML 家族 sink', () => {
    const files = walk(resolve(import.meta.dirname, '.'))
    // 顶层 + model + nodes 三面的存在性下限,防扫描面静默回缩。
    expect(files.some((f) => f.includes('/model/'))).toBe(true)
    expect(files.some((f) => f.includes('/nodes/'))).toBe(true)
    expect(files.length).toBeGreaterThan(12)
    for (const f of files) {
      const src = readFileSync(f, 'utf8')
      for (const banned of BANNED) {
        expect(src.includes(banned), `${f} 含 ${banned}`).toBe(false)
      }
    }
  })
})
