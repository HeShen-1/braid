// UX-F1-c：真 API 总数/创建样本、双主题、实绘图形/焦点；零模型调用。
import { expect, test, type Page } from '@playwright/test'
import { createRequire } from 'node:module'
import { writeFile } from 'node:fs/promises'
import { TID, tid } from '../src/ui/testids.ts'
import type { ExecutionSummary } from '../src/api.ts'

const { PNG } = createRequire(import.meta.url)('playwright-core/lib/utilsBundle') as {
  PNG: { sync: { read: (buffer: Buffer) => { data: Uint8Array; width: number; height: number } } }
}
const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function settle(page: Page) {
  await page.evaluate(async () => {
    await document.fonts.ready
    await Promise.all(document.getAnimations().filter((animation) => animation.effect?.getTiming().iterations !== Infinity).map((animation) => animation.finished.catch(() => null)))
  })
}

async function overviewContrast(page: Page) {
  return page.evaluate(() => {
    const context = document.createElement('canvas').getContext('2d')!
    const rgba = (value: string) => {
      context.clearRect(0, 0, 1, 1); context.fillStyle = value; context.fillRect(0, 0, 1, 1)
      return [...context.getImageData(0, 0, 1, 1).data].map((v, i) => i === 3 ? v / 255 : v)
    }
    const over = (top: number[], base: number[]) => top.slice(0, 3).map((v, i) => v * top[3] + base[i] * (1 - top[3]))
    const luminance = (rgb: number[]) => rgb.slice(0, 3).map((v) => v / 255 <= 0.04045 ? v / 255 / 12.92 : ((v / 255 + 0.055) / 1.055) ** 2.4).reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0)
    const ratio = (front: number[], back: number[]) => { const a = luminance(over(front, back)), b = luminance(back); return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05) }
    const background = (element: Element) => {
      const ancestors: Element[] = []
      for (let at: Element | null = element; at !== null; at = at.parentElement) ancestors.unshift(at)
      return ancestors.reduce((base, at) => [...over(rgba(getComputedStyle(at).backgroundColor), base), 1], [255, 255, 255, 1])
    }
    const samples = [...document.querySelectorAll('.exec-overview :is(h2,h3,p,dt,dd,span,time,summary,th,td,button,rect,line)')].filter((element) => element.getClientRects().length > 0).map((element) => {
      const style = getComputedStyle(element)
      const graphic = element.tagName === 'rect' || element.tagName === 'line'
      const color = rgba(element.tagName === 'rect' ? style.fill : element.tagName === 'line' ? style.stroke : style.color)
      return { text: element.textContent?.slice(0, 70), kind: element.tagName, minimum: graphic ? 3 : 4.5, ratio: ratio(color, background(element)) }
    })
    const active = document.activeElement as HTMLElement
    const style = getComputedStyle(active)
    const back = background(active)
    const bar = document.querySelector('.exec-overview-bar')!
    return { samples, barRGB: over(rgba(getComputedStyle(bar).fill), background(bar)), focus: { visible: active.matches(':focus-visible'), style: style.outlineStyle, width: style.outlineWidth, ratio: ratio(rgba(style.outlineColor), back), rgb: over(rgba(style.outlineColor), back) } }
  })
}

test('执行总览：当前租户真实总数、最近100条创建分布、筛选独立与双主题键盘', async ({ page, request }, info) => {
  test.setTimeout(180_000)
  const auth = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(auth.ok()).toBe(true)
  const headers = { Authorization: `Bearer ${(await auth.json() as { token: string }).token}` }
  const name = `uxf1c-overview-${Date.now()}`
  const draft = await request.post('/api/v1/definitions', { headers, data: { dsl: '1.0', name, nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], instructions: '执行总览真实样本', outputs: { decision: 'string' } }], edges: [] } })
  expect(draft.ok()).toBe(true)
  const published = await request.post(`/api/v1/drafts/${(await draft.json() as { id: string }).id}/publish`, { headers })
  expect(published.ok()).toBe(true)
  const version = (await published.json() as { version: number }).version
  const existing = await request.get('/api/v1/executions?limit=1&offset=0', { headers })
  expect(existing.ok()).toBe(true)
  const seedCount = Math.max(2, 101 - (await existing.json() as { page: { total: number } }).page.total)
  const ids: string[] = []
  for (let start = 0; start < seedCount; start += 8) {
    await Promise.all(Array.from({ length: Math.min(8, seedCount - start) }, async (_, offset) => {
      const triggered = await request.post('/api/v1/executions', { headers: { ...headers, 'Idempotency-Key': `${name}-${start + offset}` }, data: { definition_name: name, version, input: {} } })
      expect(triggered.ok()).toBe(true)
      ids.push((await triggered.json() as { id: string }).id)
    }))
  }
  await expect.poll(async () => {
    const read = await request.get(`/api/v1/executions?name=${name}&status=WAITING_APPROVAL&limit=1&offset=0`, { headers })
    expect(read.ok()).toBe(true)
    return (await read.json() as { page: { total: number } }).page.total
  }, { timeout: 60_000 }).toBe(seedCount)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await page.getByTestId(TID.navExecutions).click()
  const overview = page.getByRole('region', { name: '执行总览', exact: true })
  const refresh = overview.getByRole('button', { name: '刷新执行总览' })
  await expect(refresh).toBeEnabled()
  const reads = ['', 'RUNNING', 'WAITING_APPROVAL', 'FAILED'].map((status) => page.waitForResponse((response) => {
    const url = new URL(response.url())
    return url.pathname === '/api/v1/executions' && url.searchParams.get('limit') === (status === '' ? '100' : '1') && (url.searchParams.get('status') ?? '') === status
  }))
  await refresh.click()
  const payloads = await Promise.all((await Promise.all(reads)).map((response) => { expect(response.ok()).toBe(true); return response.json() as Promise<{ page: { total: number }; items: ExecutionSummary[] }> }))
  expect(payloads[0].page.total).toBeGreaterThan(100)
  expect(payloads[0].items).toHaveLength(100)
  for (const [index, label] of ['保留执行总数', '执行中', '等待审批', '已失败'].entries()) await expect(overview.getByLabel(`${label}：${payloads[index].page.total}条`, { exact: true })).toBeVisible()
  const validTimes = payloads[0].items.map((item) => Date.parse(item.created_at)).filter(Number.isFinite)
  await expect(overview).toContainText(`已取 ${payloads[0].items.length} 条 · 有效时间 ${validTimes.length} 条`)
  await expect(overview.getByRole('img')).toBeVisible()
  await expect(overview).toContainText(`${new Date(Math.min(...validTimes)).toISOString().replace('T', ' ').replace('Z', '')} 至 ${new Date(Math.max(...validTimes)).toISOString().replace('T', ' ').replace('Z', '')}`)
  const sum = await overview.locator('.exec-overview-buckets tbody tr td:last-child').allTextContents()
  expect(sum.reduce((total, value) => total + Number(value), 0)).toBe(validTimes.length)
  const list = page.getByRole('region', { name: '执行列表', exact: true })
  await list.getByLabel('按名称', { exact: true }).fill(name)
  await expect(list.getByTestId(TID.execTable).locator('tbody tr')).toHaveCount(Math.min(seedCount, 20))
  await expect(overview.getByLabel(`保留执行总数：${payloads[0].page.total}条`, { exact: true })).toBeVisible()
  await expect(overview).toContainText('不受列表筛选影响')
  const matrix: unknown[] = []
  for (const width of [1440, 1024]) for (const theme of ['dark', 'light'] as const) {
    await page.setViewportSize({ width, height: 1000 })
    await page.getByTestId(tid.themeOption(theme)).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    await expect(refresh).toBeEnabled()
    await refresh.focus()
    await page.keyboard.press('Tab')
    await page.keyboard.press('Shift+Tab')
    await expect(refresh).toBeFocused()
    await settle(page)
    const contrast = await overviewContrast(page)
    for (const sample of contrast.samples) expect(sample.ratio, `${width}/${theme}/${sample.kind}/${sample.text}`).toBeGreaterThanOrEqual(sample.minimum)
    expect(contrast.focus).toMatchObject({ visible: true, style: 'solid', width: '2px' })
    expect(contrast.focus.ratio).toBeGreaterThanOrEqual(3)
    const focus = PNG.sync.read(await refresh.screenshot({ path: info.outputPath(`overview-focus-${theme}-${width}.png`) }))
    for (const y of [0, 1]) for (let channel = 0; channel < 3; channel++) expect(Math.abs(focus.data[(y * focus.width + Math.floor(focus.width / 2)) * 4 + channel] - contrast.focus.rgb[channel])).toBeLessThanOrEqual(3)
    const barRects = await overview.locator('.exec-overview-bar').evaluateAll((elements) => elements.map((element) => ({ x: Number(element.getAttribute('x')), y: Number(element.getAttribute('y')), width: Number(element.getAttribute('width')), height: Number(element.getAttribute('height')) })))
    const paintedBar = barRects.find((rect) => rect.height > 0)!
    const chart = PNG.sync.read(await overview.getByRole('img').screenshot({ path: info.outputPath(`overview-chart-${theme}-${width}.png`) }))
    const x = Math.floor((paintedBar.x + paintedBar.width / 2) / 600 * chart.width), y = Math.floor((paintedBar.y + paintedBar.height / 2) / 100 * chart.height)
    for (let channel = 0; channel < 3; channel++) expect(Math.abs(chart.data[(y * chart.width + x) * 4 + channel] - contrast.barRGB[channel])).toBeLessThanOrEqual(3)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    expect((await list.boundingBox())!.width).toBeGreaterThan(620)
    await page.screenshot({ path: info.outputPath(`overview-${theme}-${width}.png`), fullPage: true })
    const measurement = { width, theme, contrast, paintedFocus: [0, 1].map((y) => [...focus.data.subarray((y * focus.width + Math.floor(focus.width / 2)) * 4, (y * focus.width + Math.floor(focus.width / 2)) * 4 + 3)]), paintedBar: [...chart.data.subarray((y * chart.width + x) * 4, (y * chart.width + x) * 4 + 3)], barRects, buckets: await overview.locator('.exec-overview-buckets tbody tr').allTextContents(), dom: await overview.evaluate((element) => element.outerHTML) }
    await writeFile(info.outputPath(`overview-measure-${theme}-${width}.json`), JSON.stringify(measurement, null, 2))
    matrix.push(measurement)
  }
  await page.route('**/api/v1/executions?**', async (route) => new URL(route.request().url()).searchParams.get('limit') === '100' ? route.fulfill({ status: 503, json: { error: 'unavailable' } }) : route.continue())
  await refresh.click()
  await expect(overview).toContainText('创建分布读取失败')
  await expect(overview).toContainText('保留')
  await expect(overview.getByLabel(`保留执行总数：${payloads[0].page.total}条`, { exact: true })).toBeVisible()
  await expect(overview.getByRole('img')).toBeVisible()
  await page.unroute('**/api/v1/executions?**')
  await refresh.click()
  await expect(overview.getByText(/创建分布读取失败/)).toHaveCount(0)
  await list.getByTestId(tid.execRow(ids.at(-1)!)).click()
  await expect(page.getByTestId(TID.execDetail)).toBeVisible()
  await expect(overview).toHaveCount(0)
  await expect(page).toHaveURL(new RegExp(`/executions/${ids.at(-1)!}$`))
  const evidence = JSON.stringify({ name, seedCount, ids, payloads, matrix }, null, 2)
  await writeFile(info.outputPath('execution-overview-real-evidence.json'), evidence)
  await info.attach('execution-overview-real-evidence', { body: evidence, contentType: 'application/json' })
})
