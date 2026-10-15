// T23.5:真实 API 的已发布 DSL 与当前画布逐字段对拍;
// 保存错误用浏览器路由注入,同时断言零 publish 与服务端仍为原草稿。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { loadBase } from '../src/model/testdata.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function seed(request: APIRequestContext, suffix: string) {
  const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const doc = { ...loadBase('minimal-llm'), name: `t235-${suffix}-${Date.now()}` }
  const created = await request.post('/api/v1/definitions', { headers, data: doc })
  expect(created.ok()).toBe(true)
  const { id } = await created.json() as { id: string }
  return { id, headers, doc }
}

async function login(page: Page, id: string) {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
  await page.goto(`/workflows/${id}`)
  await expect(page.getByTestId(TID.editor)).toBeVisible()
  await page.getByRole('button', { name: '工作流设置', exact: true }).click()
}

test('修改画布后直接发布:先保存,服务端发布版本等于所见 DSL,保留成功出口', async ({ page, request }) => {
  const { id, headers, doc } = await seed(request, 'publish')
  await login(page, id)
  await page.getByLabel('描述', { exact: true }).fill('直接发布必须包含当前编辑')
  const current = JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}')
  const writes: string[] = []
  page.on('request', (req) => {
    if (req.url().endsWith(`/drafts/${id}`) && req.method() === 'PUT') writes.push('save')
    if (req.url().endsWith(`/drafts/${id}/publish`)) writes.push('publish')
  })
  const published = page.waitForResponse((res) => res.url().endsWith(`/drafts/${id}/publish`))
  await page.getByTestId(TID.editorPublish).click()
  const response = await published
  expect(response.ok()).toBe(true)
  const { version } = await response.json() as { version: number }
  expect(writes).toEqual(['save', 'publish'])
  const stored = await request.get(`/api/v1/definitions/${doc.name}/versions/${version}`, { headers })
  expect(stored.ok()).toBe(true)
  const detail = await stored.json() as { status: string; dsl: unknown }
  expect(detail.status).toBe('PUBLISHED')
  expect(detail.dsl).toEqual(current)
  await expect(page.getByTestId(TID.editor)).toHaveCount(0)
  await expect(page.getByTestId(TID.workflowFlash)).toContainText(`已发布 ${doc.name}`)
  await page.getByTestId(TID.viewPublished).click()
  await expect(page).toHaveURL(/\/executions$/)
})

test('保存失败零发布;新建/换稿/离开/刷新取消后仍有原编辑与焦点', async ({ page, request }) => {
  const first = await seed(request, 'failure')
  const second = await seed(request, 'other')
  await login(page, first.id)
  const description = '保存失败也保留当前编辑'
  await page.getByLabel('描述', { exact: true }).fill(description)
  let publishCount = 0
  page.on('request', (req) => { if (req.url().endsWith(`/drafts/${first.id}/publish`)) publishCount++ })
  await page.route(`**/api/v1/drafts/${first.id}`, async (route) => {
    if (route.request().method() !== 'PUT') { await route.continue(); return }
    await route.fulfill({ status: 400, json: { error: 'invalid_dsl', issues: [{ code: 'invalid_dsl', detail: '本次保存失败' }] } })
  })
  await page.getByTestId(TID.editorPublish).click()
  await expect(page.getByTestId(TID.issues)).toContainText('本次保存失败')
  expect(publishCount).toBe(0)
  await expect(page.getByLabel('描述', { exact: true })).toHaveValue(description)
  const stored = await request.get(`/api/v1/drafts/${first.id}`, { headers: first.headers })
  const detail = await stored.json() as { status: string; dsl: unknown }
  expect(detail.status).toBe('DRAFT')
  expect(detail.dsl).toEqual(first.doc)

  await page.getByTestId(TID.draftNew).click()
  await expect(page.getByRole('group', { name: '新建前确认' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.draftNew)).toBeFocused()
  await expect(page.getByLabel('描述', { exact: true })).toHaveValue(description)

  await page.getByTestId(tid.draftItem(second.id)).click()
  await expect(page.getByRole('group', { name: '未保存的画布' })).toBeVisible()
  await page.getByRole('button', { name: '放弃', exact: true }).click()
  await expect(page).toHaveURL(new RegExp(`/workflows/${first.id}$`))
  await expect(page.getByTestId(tid.draftItem(second.id))).toBeFocused()
  await expect(page.getByLabel('描述', { exact: true })).toHaveValue(description)

  await page.getByTestId(TID.navExecutions).click()
  await expect(page.getByRole('group', { name: '未保存的画布' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.navExecutions)).toBeFocused()

  const dialog = page.waitForEvent('dialog')
  const reload = page.reload().catch(() => null)
  const beforeUnload = await dialog
  expect(beforeUnload.type()).toBe('beforeunload')
  await beforeUnload.dismiss()
  await reload
  await expect(page.getByLabel('描述', { exact: true })).toHaveValue(description)
  expect(publishCount).toBe(0)

  await page.getByTestId(TID.logout).click()
  await expect(page.getByRole('group', { name: '退出前确认' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.logout)).toBeFocused()
  await expect(page.getByLabel('描述', { exact: true })).toHaveValue(description)
  await page.getByTestId(TID.logout).click()
  await page.getByRole('button', { name: '仍要退出' }).click()
  await expect(page.getByTestId(TID.loginUsername)).toBeVisible()
})
