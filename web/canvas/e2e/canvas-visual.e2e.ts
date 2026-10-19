// UX-F1-b/#244:画布分面默认浅色、类型色与双主题可读性。
// 只核浏览器真实渲染；DSL 仍由现有转换/保存旅程负责。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { loadBase } from '../src/model/testdata.ts'
import { TID } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function seed(request: APIRequestContext) {
  const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const doc = { ...loadBase('all-node-types'), name: `ux-f1-b-${Date.now()}` }
  const created = await request.post('/api/v1/definitions', { headers, data: doc })
  expect(created.ok(), await created.text()).toBe(true)
  const { id } = await created.json() as { id: string }
  return { id, doc }
}

async function open(page: Page, id: string) {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navWorkflows)).toBeVisible()
  await page.goto(`/workflows/${id}`)
  await expect(page.getByTestId(TID.editor)).toBeVisible()
}

function contrast(background: string, foreground: string): number {
  const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
  const luminance = (value: string) => rgb(value).map((channel) => channel / 255).map((channel) =>
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4,
  ).reduce((sum, channel, index) => sum + channel * [0.2126, 0.7152, 0.0722][index], 0)
  const a = luminance(background)
  const b = luminance(foreground)
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
}

async function renderedTypes(page: Page) {
  return page.locator('.bn[data-type]').evaluateAll((cards) => cards.map((card) => {
    const style = getComputedStyle(card)
    return { type: card.getAttribute('data-type'), background: style.backgroundColor, foreground: style.color }
  }))
}

async function renderedText(page: Page) {
  return page.locator('.bn[data-type]').evaluateAll((cards) => cards.flatMap((card) => {
    const background = getComputedStyle(card).backgroundColor
    return [...card.querySelectorAll<HTMLElement>('.bn-type, .bn-title, .bn-id, .bn-line, .bn-port')]
      .filter((element) => element.textContent?.trim() !== '')
      .map((element) => ({ label: element.textContent?.trim(), background, foreground: getComputedStyle(element).color }))
  }))
}

test('画布无偏好默认浅色，五类节点彩色且双主题对比度达标', async ({ page, request }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const { id, doc } = await seed(request)
  await open(page, id)

  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  await expect.poll(() => page.locator('.react-flow').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(247, 249, 251)')
  const initial = JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}')
  expect(initial.nodes).toEqual(doc.nodes)
  expect(initial.edges).toEqual(doc.edges)

  for (const theme of ['light', 'dark'] as const) {
    await page.getByTestId(TID.themeControl).getByTestId(`theme-option-${theme}`).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    await expect.poll(() => page.locator('.react-flow').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe(
      theme === 'light' ? 'rgb(247, 249, 251)' : 'rgb(8, 9, 10)',
    )
    await expect.poll(async () => (await renderedTypes(page)).every((card) => contrast(card.background, card.foreground) >= 4.5)).toBe(true)
    await expect.poll(async () => (await renderedText(page)).every((item) => contrast(item.background, item.foreground) >= 4.5)).toBe(true)
    const cards = await renderedTypes(page)
    expect(new Set(cards.map((card) => card.type))).toEqual(new Set(['llm', 'tool', 'branch', 'approval', 'parallel']))
    expect(new Set(cards.map((card) => card.background)).size).toBe(5)
    for (const card of cards) expect(contrast(card.background, card.foreground), `${theme}/${card.type}`).toBeGreaterThanOrEqual(4.5)
    await page.screenshot({ path: test.info().outputPath(`canvas-${theme}.png`), fullPage: true, animations: 'disabled' })
  }

  await page.getByTestId(TID.themeControl).getByTestId('theme-option-light').click()
  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  await page.getByTestId(TID.themeControl).getByTestId('theme-option-dark').click()
  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
})
