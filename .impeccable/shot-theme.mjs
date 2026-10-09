// T21.3 主题 / 折叠核验截图(D-2026-10-09-console-v2 决策 1 + 6)。
//
// 阶段 3 起走**真控件**:不再直接改 <html data-theme>,而是点壳里的三态按钮
// (theme-option-*),点完读回属性断言 —— 截图因此也是控制链路的证据;顺带
// 用刷新验证 localStorage 偏好经首绘前启动脚本(assets/theme-boot.js)落定。
// 桌面 1600×1000 / 窄窗 1180×900(不做移动端,窄窗只保证不横向溢出),
// 每面各拍深/浅两版,另拍两栏折叠态。
//
// 用法:BRAID_BASE=http://127.0.0.1:8081 node .impeccable/shot-theme.mjs [out-dir]
// 依赖 web/canvas/node_modules 里的 playwright-core(与 shot.mjs 同源)。
// testid 真源是 web/canvas/src/ui/testids.ts;本脚本是纯 .mjs(不进 TS 构建),
// 故按 shot.mjs 的既有口径写字面量。
import { chromium } from '../web/canvas/node_modules/playwright-core/index.mjs'
import { mkdir } from 'node:fs/promises'
import path from 'node:path'

const BASE = process.env.BRAID_BASE ?? 'http://127.0.0.1:8081'
const USER = process.env.BRAID_USER ?? 'alice'
const PASS = process.env.BRAID_PASS ?? 'dev-author-1'
const outDir = process.argv[2] ?? '/tmp/braid-theme-shots3'

const THEMES = ['dark', 'light']
const NARROW = { width: 1180, height: 900 }

await mkdir(outDir, { recursive: true })

async function shot(page, name) {
  await page.waitForTimeout(450)
  const file = path.join(outDir, `${name}.png`)
  await page.screenshot({ path: file })
  console.log(`wrote ${file}`)
}

/** 点真控件,并读回属性 —— 控件没接线就立刻红,不产出假截图。 */
async function setTheme(page, theme) {
  await page.getByTestId(`theme-option-${theme}`).click()
  const got = await page.evaluate(() => document.documentElement.dataset.theme)
  if (got !== theme) throw new Error(`主题控件未落定:data-theme=${got}, want ${theme}`)
  const pressed = await page.getByTestId(`theme-option-${theme}`).getAttribute('aria-pressed')
  if (pressed !== 'true') throw new Error(`主题控件 aria-pressed=${pressed}, want true`)
  await page.waitForTimeout(350)
}

async function login(ctx) {
  const page = await ctx.newPage()
  page.on('pageerror', (e) => console.log(`[pageerror] ${e.message}`))
  page.on('console', (m) => {
    if (m.type() === 'error') console.log(`[console.error] ${m.text()}`)
  })
  await page.goto(`${BASE}/workflows/`, { waitUntil: 'networkidle' })
  await page.getByTestId('login-username').fill(USER)
  await page.getByTestId('login-password').fill(PASS)
  await page.getByTestId('login-submit').click()
  await page.waitForSelector('[data-testid="shell"]', { timeout: 20000 })
  return page
}

/** 三个面各拍深/浅两版;返回时停在审批面。 */
async function walkSurfaces(page, prefix) {
  // 工作流面:打开第一条草稿 → 编辑器(两栏默认展开)
  await page.getByTestId('nav-workflows').click()
  await page.waitForTimeout(1200)
  const draft = page.locator('[data-testid^="draft-item-"]').first()
  if ((await draft.count()) > 0) {
    await draft.click()
    await page.waitForTimeout(1600)
  }
  for (const theme of THEMES) {
    await setTheme(page, theme)
    await shot(page, `${prefix}-workflows-${theme}`)
  }

  // 两栏折叠:左右各收一次(折叠态进 localStorage,刷新后仍是收起的)
  await setTheme(page, 'dark')
  await page.getByTestId('panel-left-toggle').click()
  await page.getByTestId('panel-right-toggle').click()
  await shot(page, `${prefix}-workflows-collapsed-dark`)
  await page.reload({ waitUntil: 'networkidle' })
  await page.waitForSelector('[data-testid="shell"]', { timeout: 20000 })
  await page.waitForTimeout(1200)
  const label = await page.getByTestId('panel-left-toggle').getAttribute('aria-label')
  if (label !== '展开工作流列表') throw new Error('折叠态没有跨刷新保持(localStorage 未生效)')
  await shot(page, `${prefix}-workflows-collapsed-after-reload-dark`)
  await page.getByTestId('panel-left-toggle').click()
  await page.getByTestId('panel-right-toggle').click()
  await page.waitForTimeout(600)

  // 执行面:点开第一条执行(时间带 + 处置条)
  await page.getByTestId('nav-executions').click()
  await page.waitForTimeout(1200)
  const row = page.locator('[data-testid^="exec-row-"]').first()
  if ((await row.count()) > 0) {
    await row.click()
    await page.waitForTimeout(1500)
  }
  for (const theme of THEMES) {
    await setTheme(page, theme)
    await shot(page, `${prefix}-executions-${theme}`)
  }

  // 审批面:队列 + 详情
  await page.getByTestId('nav-approvals').click()
  await page.waitForTimeout(1200)
  const appr = page.locator('[data-testid^="approval-row-"]').first()
  if ((await appr.count()) > 0) {
    await appr.click()
    await page.waitForTimeout(1400)
  }
  for (const theme of THEMES) {
    await setTheme(page, theme)
    await shot(page, `${prefix}-approvals-${theme}`)
  }
}

const browser = await chromium.launch()

// ---- 桌面 ----
const desktop = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 1 })
const page = await login(desktop)
await walkSurfaces(page, 'desktop')
await desktop.close()

// ---- 窄窗(独立上下文,仅改视口) ----
const narrowCtx = await browser.newContext({ viewport: NARROW, deviceScaleFactor: 1 })
const narrow = await login(narrowCtx)
await walkSurfaces(narrow, 'narrow')
await narrowCtx.close()

// ---- 三态 × 系统偏好:auto 必须真的跟随 prefers-color-scheme(渲染层,不是属性层)----
{
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 800 }, deviceScaleFactor: 1 })
  const page = await login(ctx)
  const probe = () =>
    page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement)
      return { ground0: cs.getPropertyValue('--color-ground-0').trim(), colorScheme: cs.colorScheme }
    })
  const DARK = '#0a0e13'
  const LIGHT = '#f7f9fb'
  for (const [theme, sys, want] of [
    ['dark', 'light', DARK],
    ['light', 'dark', LIGHT],
    ['auto', 'dark', DARK],
    ['auto', 'light', LIGHT],
  ]) {
    await page.getByTestId(`theme-option-${theme}`).click()
    await page.emulateMedia({ colorScheme: sys })
    await page.waitForTimeout(300)
    const got = await probe()
    if (got.ground0 !== want)
      throw new Error(`主题 ${theme} + 系统 ${sys}:ground-0=${got.ground0}, want ${want}(auto 没跟随系统?)`)
    console.log(`theme=${theme} + 系统 ${sys} → ground-0=${got.ground0} color-scheme=${got.colorScheme} OK`)
  }
  await page.emulateMedia({ colorScheme: null })
  await ctx.close()
}

// ---- 登录面(无会话):验证偏好经首绘前启动脚本落定 ----
for (const theme of THEMES) {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 1 })
  // 只写偏好,不碰 data-theme:属性必须由 assets/theme-boot.js 在首绘前落定。
  await ctx.addInitScript((t) => {
    try {
      localStorage.setItem('braid.theme', t)
    } catch {
      /* 隐私模式:下面断言会红,属预期 */
    }
  }, theme)
  const fresh = await ctx.newPage()
  await fresh.goto(`${BASE}/login`, { waitUntil: 'domcontentloaded' })
  const at = await fresh.evaluate(() => document.documentElement.dataset.theme)
  if (at !== theme) throw new Error(`启动脚本未落定 data-theme:${at}, want ${theme}`)
  await fresh.waitForTimeout(900)
  await shot(fresh, `desktop-login-${theme}`)
  await ctx.close()
}

await browser.close()
console.log('done ->', outDir)
