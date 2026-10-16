// UX-F1-d:真实 API 状态/name 过滤与列表键盘选择;视觉矩阵只覆盖执行列表。
// 不以当前页推算状态计数,审批节点不调用模型。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function seed(request: APIRequestContext) {
  const auth = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(auth.ok()).toBe(true)
  const { token } = await auth.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const name = `uxf1d-list-${Date.now()}`
  const ids: string[] = []
  for (const [workflow, count] of [[name, 3], [`${name}-other`, 1]] as const) {
    const draft = await request.post('/api/v1/definitions', {
      headers,
      data: { dsl: '1.0', name: workflow, nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], instructions: '执行列表视觉验收', outputs: { decision: 'string' } }], edges: [] },
    })
    expect(draft.ok()).toBe(true)
    const draftId = (await draft.json() as { id: string }).id
    const published = await request.post(`/api/v1/drafts/${draftId}/publish`, { headers })
    expect(published.ok()).toBe(true)
    const { version } = await published.json() as { version: number }
    for (let i = 0; i < count; i++) {
      const triggered = await request.post('/api/v1/executions', {
        headers: { ...headers, 'Idempotency-Key': `${workflow}-${i}` },
        data: { definition_name: workflow, version, input: {} },
      })
      expect(triggered.ok()).toBe(true)
      const id = (await triggered.json() as { id: string }).id
      await expect.poll(async () => {
        const response = await request.get(`/api/v1/executions/${id}`, { headers })
        expect(response.ok()).toBe(true)
        return (await response.json() as { status: string }).status
      }, { timeout: 30_000 }).toBe('WAITING_APPROVAL')
      if (workflow === name) ids.push(id)
    }
  }
  const cancelled = await request.post(`/api/v1/executions/${ids[0]}/cancel`, { headers })
  expect(cancelled.ok()).toBe(true)
  return { name, ids }
}

async function listContrast(page: Page) {
  return page.evaluate(() => {
    const canvas = document.createElement('canvas')
    canvas.width = 1; canvas.height = 1
    const context = canvas.getContext('2d')!
    const rgba = (color: string) => {
      context.clearRect(0, 0, 1, 1)
      context.fillStyle = color
      context.fillRect(0, 0, 1, 1)
      return Array.from(context.getImageData(0, 0, 1, 1).data).map((v, i) => i === 3 ? v / 255 : v)
    }
    const over = (top: number[], base: number[]) => top.slice(0, 3).map((v, i) => v * top[3] + base[i] * (1 - top[3]))
    const luminance = (color: number[]) => color.slice(0, 3).map((v) => {
      const s = v / 255
      return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
    }).reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0)
    const contrast = (foreground: number[], background: number[]) => {
      const a = luminance(over(foreground, background)), b = luminance(background)
      return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
    }
    const selectors = '.exec-list .tab, .exec-list .field-label, .exec-list .field-hint, .exec-list .input, .exec-table th, .exec-table .exec-row-id, .exec-row-name > span, .exec-table .chip, .exec-table time, .exec-table-hint'
    return Array.from(document.querySelectorAll<HTMLElement>(selectors)).map((element) => {
      const ancestors: Element[] = []
      for (let at: Element | null = element; at !== null; at = at.parentElement) ancestors.unshift(at)
      const background = ancestors.reduce((base, at) => [...over(rgba(getComputedStyle(at).backgroundColor), base), 1], [255, 255, 255, 1])
      const style = getComputedStyle(element)
      const samples = [{ kind: 'text', ratio: contrast(rgba(style.color), background) }]
      if (element instanceof HTMLInputElement) samples.push({ kind: 'placeholder', ratio: contrast(rgba(getComputedStyle(element, '::placeholder').color), background) })
      return { tag: element.tagName, text: element.textContent?.slice(0, 60), samples }
    })
  })
}

test('执行列表:真实状态tab/精确name、双主题密表、窄窗与键盘选择', async ({ page, request }, info) => {
  const { name, ids } = await seed(request)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await page.getByTestId(TID.navExecutions).click()
  const list = page.getByRole('region', { name: '执行列表', exact: true })
  const nameInput = list.getByLabel('按名称', { exact: true })
  await nameInput.fill(name)
  await expect(list.getByTestId(TID.execTable).locator('tbody tr')).toHaveCount(3)
  await expect(list.getByRole('columnheader')).toHaveText(['执行 ID', '工作流 / 版本', '状态', '时长', '创建'])
  const waiting = list.getByRole('tab', { name: '等待审批', exact: true })
  const filtered = page.waitForResponse((response) => {
    const url = new URL(response.url())
    return url.pathname === '/api/v1/executions' && url.searchParams.get('limit') === '20' && url.searchParams.get('status') === 'WAITING_APPROVAL' && url.searchParams.get('name') === name
  })
  await waiting.click()
  expect((await filtered).ok()).toBe(true)
  await expect(list.getByTestId(TID.execTable).locator('tbody tr')).toHaveCount(2)
  await expect(list.getByTestId(tid.execRow(ids[0]))).toHaveCount(0)
  await expect(waiting).toHaveAttribute('aria-selected', 'true')
  await waiting.focus()
  await page.keyboard.press('ArrowRight')
  await expect(list.getByRole('tab', { name: '已成功', exact: true })).toBeFocused()
  await expect(list.getByTestId(TID.execEmpty)).toContainText('当前筛选下没有执行')
  await page.keyboard.press('Home')
  await expect(list.getByRole('tab', { name: '全部执行' })).toBeFocused()
  await expect(list.getByTestId(TID.execTable).locator('tbody tr')).toHaveCount(3)
  await nameInput.fill(name.slice(0, -1))
  await expect(list.getByTestId(TID.execEmpty)).toContainText('当前筛选下没有执行')
  await nameInput.fill(name)
  await expect(list.getByTestId(TID.execTable).locator('tbody tr')).toHaveCount(3)
  const matrix: unknown[] = []
  for (const width of [1440, 1024]) {
    await page.setViewportSize({ width, height: 1000 })
    for (const theme of ['dark', 'light'] as const) {
      await page.getByTestId(tid.themeOption(theme)).click()
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
      await page.evaluate(() => document.fonts.ready)
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
      const contrast = await listContrast(page)
      for (const sample of contrast) for (const value of sample.samples) expect(value.ratio, `${width}/${theme}/${sample.text}/${value.kind}`).toBeGreaterThanOrEqual(4.5)
      const geometry = await list.boundingBox()
      expect(geometry!.width).toBeGreaterThan(620)
      await page.screenshot({ path: info.outputPath(`execution-list-${theme}-${width}.png`), fullPage: true, animations: 'disabled' })
      matrix.push({ width, theme, geometry, contrast })
    }
  }
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.getByTestId(tid.themeOption('dark')).click()
  const row = list.getByTestId(tid.execRow(ids[1]))
  await expect(row).toContainText(`#${ids[1]}`)
  await row.focus()
  await page.keyboard.press('Space')
  await expect(page).toHaveURL(new RegExp(`/executions/${ids[1]}$`))
  await expect(row).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'WAITING_APPROVAL')
  await expect(page.getByTestId(TID.execDetail)).toBeVisible()
  await page.screenshot({ path: info.outputPath('execution-list-selected-dark-1440.png'), fullPage: true, animations: 'disabled' })
  await info.attach('execution-list-real-evidence', { body: JSON.stringify({ name, ids, matrix }), contentType: 'application/json' })
})
