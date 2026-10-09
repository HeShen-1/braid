// 渲染面实测(浏览器):把 DESIGN.md 的「定档规则」从 token 对推进到**真实渲染**。
//
// token 对由 `.impeccable/contrast.mjs` 静态算出(契约面);本脚本在真机上按
// 计算样式采样,回答两个静态算不出的问题:
//   ① 正文/标签实际落在哪个背景上(color vs 逐层合成后的背景);
//   ② 交互控件描边(1px)实际邻接哪个背景(非文本图形 ≥3:1 只对真实邻接成立)。
//
// 背景一律用 1×1 canvas 逐层 fillRect 合成 = 浏览器自己的合成结果 —— 芯片底色
// 是 color-mix(in oklab, …) 与半透明 tint,字面 rgba 解析会漏(踩过)。
//
// 深/浅两套各跑一遍,打印每面的「最紧正文比 + 欠档数」与交互描边邻接。
// 需要 dev 栈在跑(默认 127.0.0.1:8081)。用法:node .impeccable/contrast-render.mjs
import { chromium } from '../web/canvas/node_modules/playwright-core/index.mjs'

const BASE = process.env.BRAID_BASE ?? 'http://127.0.0.1:8081'
const USER = process.env.BRAID_USER ?? 'alice'
const PASS = process.env.BRAID_PASS ?? 'dev-author-1'
const LINE = 4.5 // 正文线
const GRAPHIC = 3 // 非文本图形线

const lum = (rgb) => {
  const f = (c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4)
  return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2])
}
const ratio = (a, b) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p)
  return (x + 0.05) / (y + 0.05)
}
const hex = (rgb) => '#' + rgb.map((v) => Math.round(v * 255).toString(16).padStart(2, '0')).join('')

const IN_PAGE = () => {
  const cv = document.createElement('canvas')
  cv.width = cv.height = 1
  const cx = cv.getContext('2d')
  const paint = (color) => {
    try {
      cx.fillStyle = color
    } catch {
      return
    }
    cx.fillRect(0, 0, 1, 1)
  }
  /** 单色(不透明)解析:画在白底上取像素。 */
  const solid = (color) => {
    cx.clearRect(0, 0, 1, 1)
    paint('#ffffff')
    paint(color)
    const d = cx.getImageData(0, 0, 1, 1).data
    return [d[0] / 255, d[1] / 255, d[2] / 255]
  }
  /** 背景合成:自内向外逐层 fillRect,得真实渲染色。 */
  const bgOf = (el) => {
    const chain = []
    let n = el
    while (n) {
      const c = getComputedStyle(n).backgroundColor
      if (c && c !== 'rgba(0, 0, 0, 0)' && c !== 'transparent') chain.push(c)
      n = n.parentElement
    }
    cx.clearRect(0, 0, 1, 1)
    paint('#ffffff')
    for (const c of [...chain].reverse()) paint(c)
    const d = cx.getImageData(0, 0, 1, 1).data
    return [d[0] / 255, d[1] / 255, d[2] / 255]
  }
  const texts = []
  const borders = []
  for (const el of document.querySelectorAll('*')) {
    const tag = el.tagName
    if (tag === 'SCRIPT' || tag === 'STYLE' || el.closest('svg')) continue
    const cs = getComputedStyle(el)
    if (cs.visibility === 'hidden' || cs.display === 'none' || parseFloat(cs.opacity) < 0.95) continue
    const rect = el.getBoundingClientRect()
    if (rect.width < 2 || rect.height < 2) continue
    const disabled = el.closest('[disabled],[aria-disabled="true"],[data-disabled]') !== null
    const text = [...el.childNodes]
      .filter((n) => n.nodeType === 3)
      .map((n) => n.textContent.trim())
      .join('')
    if (text) texts.push({ text: text.slice(0, 20), fg: solid(cs.color), bg: bgOf(el), disabled })
    const bw = parseFloat(cs.borderTopWidth) || parseFloat(cs.borderLeftWidth)
    if (bw > 0 && cs.borderTopColor !== 'rgba(0, 0, 0, 0)')
      borders.push({ border: solid(cs.borderTopColor), bg: bgOf(el.parentElement ?? el) })
  }
  const line2 = solid(getComputedStyle(document.documentElement).getPropertyValue('--color-line-2').trim() || '#000')
  return { texts, borders, line2 }
}

const browser = await chromium.launch()
const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } })
const page = await ctx.newPage()
await page.goto(`${BASE}/workflows/`, { waitUntil: 'networkidle' })
await page.getByTestId('login-username').fill(USER)
await page.getByTestId('login-password').fill(PASS)
await page.getByTestId('login-submit').click()
await page.waitForSelector('[data-testid="shell"]', { timeout: 20000 })
await page.waitForTimeout(1200)
const draft = page.locator('[data-testid^="draft-item-"]').first()
if ((await draft.count()) > 0) {
  await draft.click()
  await page.waitForTimeout(1500)
}

const isLine2 = (rgb, line2) => rgb.every((v, i) => Math.abs(v - line2[i]) < 0.02)

for (const theme of ['dark', 'light']) {
  await page.getByTestId(`theme-option-${theme}`).click()
  await page.waitForTimeout(400)
  console.log(`\n########## ${theme} ##########`)
  for (const [label, go] of [
    ['workflows/editor', async () => {}],
    ['executions', async () => (await page.getByTestId('nav-executions').click(), page.waitForTimeout(1600))],
    ['approvals', async () => (await page.getByTestId('nav-approvals').click(), page.waitForTimeout(1600))],
  ]) {
    await go()
    const { texts, borders, line2 } = await page.evaluate(IN_PAGE)
    const scored = texts
      .filter((t) => !t.disabled)
      .map((t) => ({ ...t, r: ratio(t.fg, t.bg) }))
      .sort((a, b) => a.r - b.r)
    const weak = scored.filter((t) => t.r < LINE)
    console.log(
      `-- ${label}: 正文样本 ${scored.length},最紧 ${scored[0]?.r.toFixed(2)}:1 "${scored[0]?.text}" <${LINE}:1 欠档 ${weak.length}`,
    )
    for (const w of weak.slice(0, 4)) console.log(`     ! ${w.r.toFixed(2)}:1 "${w.text}" ${hex(w.fg)} on ${hex(w.bg)}`)

    // line-2 = 交互控件描边与连线(唯一纳入非文本 3:1 的描边);其余是结构发丝线,
    // 分隔性图形不适用 1.4.11,单列不计。
    const groups = {}
    for (const b of borders) {
      const key = `${isLine2(b.border, line2) ? 'line-2' : 'hairline'} on ${hex(b.bg)}`
      groups[key] ??= { n: 0, r: ratio(b.border, b.bg) }
      groups[key].n += 1
    }
    const inScope = Object.entries(groups).filter(([k]) => k.startsWith('line-2'))
    const structural = Object.entries(groups).filter(([k]) => k.startsWith('hairline'))
    const worst = inScope.length ? Math.min(...inScope.map(([, v]) => v.r)) : NaN
    console.log(
      `   交互描边 line-2 邻接 ${inScope.length} 种,最紧 ${worst.toFixed(2)}:1` +
        (worst < GRAPHIC ? ' ← 欠档' : '') +
        `;结构发丝线 ${structural.length} 种(分隔性图形,不计)`,
    )
  }
}

await browser.close()
