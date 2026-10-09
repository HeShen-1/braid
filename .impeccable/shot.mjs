// 截图核验脚本(设计过程产物,入库以记录证据来源)。用法:
//   node .impeccable/shot.mjs <out-dir> [--before]
// 依赖 web/canvas/node_modules 里的 playwright-core。
//
// T21.2 起是**统一 SPA**:登录只有一处,审批台不再有独立页面与独立
// 会话 —— 所以所有面都在同一个已登录上下文里用壳内导航走过,顺带把
// 真实导航路径也跑一遍(旧脚本给审批台另开标签页,新 sessionStorage
// 会直接撞登录页)。
import { chromium } from '../web/canvas/node_modules/playwright-core/index.mjs'
import { mkdir } from 'node:fs/promises'
import path from 'node:path'

const BASE = process.env.BRAID_BASE ?? 'http://127.0.0.1:8081'
const WORKFLOWS = `${BASE}/workflows/`
const USER = process.env.BRAID_USER ?? 'alice'
const PASS = process.env.BRAID_PASS ?? 'dev-author-1'

const outDir = process.argv[2] ?? '.impeccable/review'
const before = process.argv.includes('--before')
const readme = process.argv.includes('--readme')

async function shot(page, name) {
  await page.waitForTimeout(700)
  const file = path.join(outDir, `${name}.png`)
  await page.screenshot({ path: file, fullPage: true })
  console.log(`wrote ${file}`)
}

const browser = await chromium.launch()
await mkdir(outDir, { recursive: true })

if (readme) {
  // README 用图:1440 宽 @1x,只拍两张最能说明问题的面(编辑器 / 执行时间带)。
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 })
  const p = await ctx.newPage()
  await p.goto(WORKFLOWS, { waitUntil: 'networkidle' })
  await p.getByTestId('login-username').fill(USER)
  await p.getByTestId('login-password').fill(PASS)
  await p.getByTestId('login-submit').click()
  await p.waitForSelector('[data-testid="shell"]', { timeout: 15000 })

  const draft = p.locator('[data-testid^="draft-item-"]').first()
  if ((await draft.count()) > 0) {
    await draft.click()
    await p.waitForTimeout(1500)
    await shot(p, 'console-workflows')
  }

  await p.getByTestId('nav-executions').click()
  await p.waitForTimeout(1200)
  const row = p.locator('[data-testid^="exec-row-"]').first()
  if ((await row.count()) > 0) {
    await row.click()
    await p.waitForTimeout(1500)
    await shot(p, 'console-executions')
  }

  // 审批面也要一张:README 的三块工作区各自可见
  await p.getByTestId('nav-approvals').click()
  await p.waitForTimeout(1200)
  const appr = p.locator('[data-testid^="approval-row-"]').first()
  if ((await appr.count()) > 0) {
    await appr.click()
    await p.waitForTimeout(1200)
  }
  await shot(p, 'console-approvals')
  await browser.close()
  process.exit(0)
}

if (before) {
  // 硬切之前的基线:旧 /ui/ 两面(只在旧提交上可用)
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 })
  const p = await ctx.newPage()
  await p.goto(`${BASE}/ui/`, { waitUntil: 'networkidle' })
  await shot(p, 'before-approval')
  await p.goto(`${BASE}/ui/canvas/`, { waitUntil: 'networkidle' })
  await shot(p, 'before-canvas-login')
  await browser.close()
  process.exit(0)
}

const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 })
const page = await ctx.newPage()
page.on('console', (m) => {
  if (m.type() === 'error') console.log(`[console.error] ${m.text()}`)
})
page.on('pageerror', (e) => console.log(`[pageerror] ${e.message}`))

// 登录(未登录访问受保护路由 → 重定向到 /login)
await page.goto(WORKFLOWS, { waitUntil: 'networkidle' })
await page.getByTestId('login-username').fill(USER)
await page.getByTestId('login-password').fill(PASS)
await page.getByTestId('login-submit').click()
await page.waitForSelector('[data-testid="shell"]', { timeout: 15000 })
await shot(page, 'desktop-workflows-empty')

// 工作流面:打开第一条草稿
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
  const ribbon = page.getByTestId('exec-ribbon')
  if ((await ribbon.count()) > 0) {
    await ribbon.screenshot({ path: path.join(outDir, 'desktop-ribbon.png') })
    console.log(`wrote ${path.join(outDir, 'desktop-ribbon.png')}`)
  }
}

// 审批面(统一 SPA:壳内导航过去,不再另开会话)
await page.getByTestId('nav-approvals').click()
await page.waitForTimeout(1200)
await shot(page, 'desktop-approvals')

const firstApproval = page.locator('[data-testid^="approval-row-"]').first()
if ((await firstApproval.count()) > 0) {
  await firstApproval.click()
  await page.waitForTimeout(1200)
  await shot(page, 'desktop-approval-detail')
}

// 窄窗(只验不横向溢出):三个面各自看一次
await page.setViewportSize({ width: 1180, height: 900 })
await shot(page, 'narrow-approvals')
await page.getByTestId('nav-executions').click()
await page.waitForTimeout(1200)
await shot(page, 'narrow-executions')
await page.getByTestId('nav-workflows').click()
await page.waitForTimeout(1000)
await shot(page, 'narrow-workflows')

// 登录面(独立上下文,不带 session)
const fresh = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 })
const p3 = await fresh.newPage()
await p3.goto(WORKFLOWS, { waitUntil: 'networkidle' })
await shot(p3, 'desktop-login')
await fresh.close()

await browser.close()
