// 对比度实测(DESIGN.md「The Measured-Grade Rule / 定档规则」的同一方法)。
//
// 为什么入库一个脚本:定档口径是「改任一 ground/ink/line/accent 的值必须用同一
// 方法重算」,所以方法本身要可复跑,而不是一次性临时脚本。两套世界的值都从
// styles/index.css 现场解析 —— 深色档 = `@theme`(默认世界的唯一定义处),
// 浅色档 = 标了 light-world:start/end 的块。
//
// 用法:node .impeccable/contrast.mjs
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const css = readFileSync(path.join(root, 'web/canvas/src/styles/index.css'), 'utf8')

/** 从一个 CSS 片段里取 `--color-<key>: <value>` 的字面量值(忽略 var() 别名)。 */
function tokens(block) {
  const out = {}
  for (const m of block.matchAll(/--color-([a-z0-9-]+):\s*([^;]+);/g)) {
    const value = m[2].trim()
    if (/^#[0-9a-fA-F]{3,8}$/.test(value)) out[m[1]] = value
  }
  return out
}

const themeBlock = css.match(/@theme \{([\s\S]*?)\n\}/)[1]
const lightBlock = css.match(/\/\* light-world:start \*\/([\s\S]*?)\/\* light-world:end \*\//)[1]
const dark = tokens(themeBlock)
const light = tokens(lightBlock)

function srgb(hex) {
  const h = hex.replace('#', '')
  const full = h.length === 3 ? h.split('').map((c) => c + c).join('') : h
  return [0, 2, 4].map((i) => parseInt(full.slice(i, i + 2), 16) / 255)
}

function luminance(hex) {
  const [r, g, b] = srgb(hex).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

/** WCAG 2.x 相对亮度对比度(不透明对)。 */
function ratio(a, b) {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p)
  return (x + 0.05) / (y + 0.05)
}

/** 成对清单 = DESIGN.md 定档规则里逐条列出的那些对(正文 ≥4.5,图形 ≥3)。 */
const PAIRS = [
  ['正文 ink-0 / 地面 ground-0', 'ink-0', 'ground-0', 4.5],
  ['正文 ink-0 / 面板 ground-1', 'ink-0', 'ground-1', 4.5],
  ['次文本 ink-1 / ground-1', 'ink-1', 'ground-1', 4.5],
  ['次文本 ink-1 / ground-2', 'ink-1', 'ground-2', 4.5],
  ['三级 ink-2 / ground-2', 'ink-2', 'ground-2', 4.5],
  ['三级 ink-2 / ground-3', 'ink-2', 'ground-3', 4.5],
  ['占位 ink-3 / ground-2', 'ink-3', 'ground-2', 4.5],
  ['占位 ink-3 / ground-3', 'ink-3', 'ground-3', 4.5],
  ['描边 line-2 / ground-1(非文本)', 'line-2', 'ground-1', 3],
  ['描边 line-2 / ground-2(非文本)', 'line-2', 'ground-2', 3],
  ['焦点 accent / ground-0', 'accent', 'ground-0', 4.5],
  ['焦点 accent / ground-1', 'accent', 'ground-1', 4.5],
  ['主操作 primary-ink / primary-action', 'primary-ink', 'primary-action', 4.5],
  ['分段选中 seg ink-0 / ground-4', 'ink-0', 'ground-4', 4.5],
  ['勾选框 check ground-1 / accent(图形)', 'ground-1', 'accent', 3],
]

const pad = (s, n) => String(s).padEnd(n, ' ')

for (const [world, toks] of [
  ['dark', dark],
  ['light', light],
]) {
  console.log(`\n== ${world} ==`)
  let worst = { text: Infinity, graphic: Infinity }
  for (const [label, fg, bg, min] of PAIRS) {
    if (!toks[fg] || !toks[bg]) {
      console.log(`  ${pad(label, 34)} ${fg}=${toks[fg] ?? '?'} ${bg}=${toks[bg] ?? '?'} 解析失败`)
      process.exitCode = 1
      continue
    }
    const r = ratio(toks[fg], toks[bg])
    const ok = r >= min
    if (!ok) process.exitCode = 1
    if (min >= 4.5) worst.text = Math.min(worst.text, r)
    else worst.graphic = Math.min(worst.graphic, r)
    console.log(
      `  ${ok ? 'OK  ' : 'FAIL'} ${pad(label, 34)} ${toks[fg]} on ${toks[bg]} = ${r.toFixed(2)}:1 (线 ${min}:1)`,
    )
  }
  console.log(`  最紧:正文 ${worst.text.toFixed(2)}:1 / 图形 ${worst.graphic.toFixed(2)}:1`)
}

// 六态芯片标签:标签色落「自色 tint(不透明)盖 ground-1」上 —— 芯片底色在
// T21.3 改成不透明,就是为了让这个背景是确定的(不随所在行变化)。
// 混色用 srgb 近似(CSS 走 oklab,真值以 contrast-render.mjs 的浏览器实测为准)。
function mix(base, fg, pct) {
  const b = srgb(base)
  const f = srgb(fg)
  return (
    '#' +
    b
      .map((c, i) => Math.round((c * (1 - pct) + f[i] * pct) * 255).toString(16).padStart(2, '0'))
      .join('')
  )
}

const CHIPS = [
  ['sig-run', 0.12],
  ['sig-ok', 0.12],
  ['sig-wait', 0.14],
  ['sig-fail', 0.12],
  ['sig-stop', 0.1],
]

console.log('\n== 六态芯片标签(色相落自色 tint(不透明,盖 ground-1),srgb 近似)==')
for (const [world, toks] of [
  ['dark', dark],
  ['light', light],
]) {
  const out = []
  for (const [key, alpha] of CHIPS) {
    const tint = mix(toks['ground-1'], toks[key], alpha)
    const r = ratio(toks[key], tint)
    if (r < 4.5) process.exitCode = 1
    out.push(`${key.replace('sig-', '')} ${r.toFixed(2)}${r < 4.5 ? '!' : ''}`)
  }
  console.log(`  ${world}: ${out.join(' · ')}:1`)
}
