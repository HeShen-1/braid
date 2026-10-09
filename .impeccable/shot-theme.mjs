// T21.3 主题核验截图:同一批面各拍深色/浅色两版(D-2026-10-09-console-v2)。
// 主题切换控件尚未实现(阶段 2),故这里直接改 <html data-theme> 来核验
// 两套 token 阶梯在真实渲染下的可读性 —— 脚本只读不写业务态。
// 用法:node .impeccable/shot-theme.mjs [out-dir]
import { chromium } from '../web/canvas/node_modules/playwright-core/index.mjs'
import { mkdir } from 'node:fs/promises'
import path from 'node:path'

const BASE = process.env.BRAID_BASE ?? 'http://127.0.0.1:8081'
const USER = process.env.BRAID_USER ?? 'alice'
const PASS = process.env.BRAID_PASS ?? 'dev-author-1'
const outDir = process.argv[2] ?? '/tmp/braid-theme-shots'

const browser = await chromium.launch()
await mkdir(outDir, { recursive: true })

async function setTheme(page, theme) {
  await page.evaluate((t) => {
    document.documentElement.setAttribute('data-theme', t)
  }, theme)
  await page.waitForTimeout(500)
}

async function shot(page, name) {
  await page.waitForTimeout(500)
  const file = path.join(outDir, `${name}.png`)
  await page.screenshot({ path: file })
  console.log(`wrote ${file}`)
}

const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 1 })
const page = await ctx.newPage()
page.on('pageerror', (e) => console.log(`[pageerror] ${e.message}`))
page.on('console', (m) => {
  if (m.type() === 'error') console.log(`[console.error] ${m.text()}`)
})

// 登录(统一 SPA:未登录访问受保护路由 → /login)
await page.goto(`${BASE}/workflows/`, { waitUntil: 'networkidle' })
await page.getByTestId('login-username').fill(USER)
await page.getByTestId('login-password').fill(PASS)
await page.getByTestId('login-submit').click()
await page.waitForSelector('[data-testid="shell"]', { timeout: 20000 })

// 导航到各面(只导一次,之后靠切 data-theme 出两版)
const surfaces = []

const draft = page.locator('[data-testid^="draft-item-"]').first()
if ((await draft.count()) > 0) {
  await draft.click()
  await page.waitForTimeout(1500)
  surfaces.push(['workflows', null])
} else {
  surfaces.push(['workflows', null])
}

await page.getByTestId('nav-executions').click()
await page.waitForTimeout(1200)
const row = page.locator('[data-testid^="exec-row-"]').first()
const hasRow = (await row.count()) > 0
if (hasRow) {
  await row.click()
  await page.waitForTimeout(1500)
}
for (const theme of ['dark', 'light']) {
  await setTheme(page, theme)
  await shot(page, `executions-${theme}`)
  const ribbon = page.getByTestId('exec-ribbon')
  if ((await ribbon.count()) > 0) {
    await ribbon.screenshot({ path: path.join(outDir, `ribbon-${theme}.png`) })
    console.log(`wrote ${path.join(outDir, `ribbon-${theme}.png`)}`)
  }
}

// 回到工作流面(导航切回)
await page.getByTestId('nav-workflows').click()
await page.waitForTimeout(1500)
const draft2 = page.locator('[data-testid^="draft-item-"]').first()
if ((await draft2.count()) > 0) {
  await draft2.click()
  await page.waitForTimeout(1800)
}
for (const theme of ['dark', 'light']) {
  await setTheme(page, theme)
  await shot(page, `workflows-${theme}`)
}

// 审批面
await page.getByTestId('nav-approvals').click()
await page.waitForTimeout(1200)
const appr = page.locator('[data-testid^="approval-row-"]').first()
if ((await appr.count()) > 0) {
  await appr.click()
  await page.waitForTimeout(1400)
}
for (const theme of ['dark', 'light']) {
  await setTheme(page, theme)
  await shot(page, `approvals-${theme}`)
}

// 登录面(独立上下文,无会话)
const fresh = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 1 })
const p2 = await fresh.newPage()
await p2.goto(`${BASE}/workflows/`, { waitUntil: 'networkidle' })
for (const theme of ['dark', 'light']) {
  await setTheme(p2, theme)
  await shot(p2, `login-${theme}`)
}
await fresh.close()
await browser.close()
console.log('done ->', outDir)
