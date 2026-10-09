// 截图核验脚本(设计过程产物,不入库)。用法:
//   node .impeccable/shot.mjs <out-dir> [--before]
// 依赖 web/canvas/node_modules 里的 playwright。
import { chromium } from '../web/canvas/node_modules/playwright-core/index.mjs'
import { mkdir } from 'node:fs/promises'
import path from 'node:path'

const BASE = process.env.BRAID_BASE ?? 'http://127.0.0.1:8081'
const CANVAS = `${BASE}/ui/canvas/`
const APPROVAL = `${BASE}/ui/`
const USER = process.env.BRAID_USER ?? 'alice'
const PASS = process.env.BRAID_PASS ?? 'dev-author-1'

const outDir = process.argv[2] ?? '.impeccable/review'
const before = process.argv.includes('--before')

async function shot(page, name) {
  await page.waitForTimeout(700)
  const file = path.join(outDir, `${name}.png`)
  await page.screenshot({ path: file, fullPage: true })
  console.log(`wrote ${file}`)
}

const browser = await chromium.launch()
const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 })
const page = await ctx.newPage()
page.on('console', (m) => {
  if (m.type() === 'error') console.log(`[console.error] ${m.text()}`)
})
page.on('pageerror', (e) => console.log(`[pageerror] ${e.message}`))

await mkdir(outDir, { recursive: true })

if (before) {
  await page.goto(APPROVAL, { waitUntil: 'networkidle' })
  await shot(page, 'before-approval')
  await page.goto(CANVAS, { waitUntil: 'networkidle' })
  await shot(page, 'before-canvas-login')
  await browser.close()
  process.exit(0)
}

// 登录
await page.goto(CANVAS, { waitUntil: 'networkidle' })
await page.getByTestId('login-username').fill(USER)
await page.getByTestId('login-password').fill(PASS)
await page.getByTestId('login-submit').click()
await page.waitForSelector('[data-testid="shell"]', { timeout: 15000 })
await shot(page, 'desktop-workflows-empty')

// 打开第一条工作流
const firstDraft = page.locator('[data-testid^="draft-item-"]').first()
if ((await firstDraft.count()) > 0) {
  await firstDraft.click()
  await page.waitForTimeout(1200)
  await shot(page, 'desktop-editor')
}

// 执行面
await page.getByTestId('nav-executions').click()
await page.waitForTimeout(1200)
await shot(page, 'desktop-executions')

const firstRow = page.locator('[data-testid^="exec-row-"]').first()
if ((await firstRow.count()) > 0) {
  await firstRow.click()
  await page.waitForTimeout(1500)
  await shot(page, 'desktop-execution-detail')
  // 时间带的单独特写
  const ribbon = page.getByTestId('exec-ribbon')
  if ((await ribbon.count()) > 0) {
    await ribbon.screenshot({ path: path.join(outDir, 'desktop-ribbon.png') })
    console.log(`wrote ${path.join(outDir, 'desktop-ribbon.png')}`)
  }
  await page.getByTestId('nav-workflows').click()
  await page.waitForTimeout(600)
  await shot(page, 'desktop-back-to-workflows')
}

// 窄窗(只验不横向溢出)——先回执行面并选中执行,让时间带在同宽度下真的渲染
await page.setViewportSize({ width: 1180, height: 900 })
await page.getByTestId('nav-executions').click()
await page.waitForTimeout(1200)
const narrowRow = page.locator('[data-testid^="exec-row-"]').first()
if ((await narrowRow.count()) > 0) {
  await narrowRow.click()
  await page.waitForTimeout(1200)
}
await shot(page, 'narrow-executions')
await page.getByTestId('nav-workflows').click()
await page.waitForTimeout(900)
await shot(page, 'narrow-workflows')

// 审批台(T21.2 前仍是原生页,只作对照)
const p2 = await ctx.newPage()
await p2.goto(APPROVAL, { waitUntil: 'networkidle' })
await shot(p2, 'desktop-approval')
await p2.close()

// 重做后的登录面(独立上下文,不带 session)
const fresh = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 })
const p3 = await fresh.newPage()
await p3.goto(CANVAS, { waitUntil: 'networkidle' })
await shot(p3, 'desktop-login')
await fresh.close()

await browser.close()
