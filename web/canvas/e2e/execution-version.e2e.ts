// T23.6:真实 API 发布/归档与浏览器钉版触发对拍,不调用 LLM 或工具。
import { expect, test, type APIRequestContext } from '@playwright/test'
import type { WorkflowDoc } from '../src/model/dsl.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function publish(request: APIRequestContext, headers: Record<string, string>, doc: WorkflowDoc) {
  const created = await request.post('/api/v1/definitions', { headers, data: doc })
  expect(created.status()).toBe(201)
  const { id } = await created.json() as { id: string }
  const response = await request.post(`/api/v1/drafts/${id}/publish`, { headers })
  expect(response.ok()).toBe(true)
  return await response.json() as { id: string; version: number; dsl_hash: string }
}

test('所选版本固定:另端发布新版与列表刷新后仍执行原版;归档拒绝且不降级', async ({ page, request }) => {
  const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const doc: WorkflowDoc = {
    dsl: '1.0',
    name: `t236-pinned-${Date.now()}`,
    description: '选择时的第一版',
    nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], instructions: '第一版审批' }],
    edges: [],
  }
  const first = await publish(request, headers, doc)

  await page.goto('/executions')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await page.getByTestId(TID.triggerDefinition).click()
  await page.getByRole('option', { name: `${doc.name} v${first.version}`, exact: true }).click()
  await expect(page.getByTestId(TID.triggerDefinition)).toContainText(`${doc.name} v${first.version}`)

  // 另一客户端在当前页面完成选择后发布新版,页面没有重新导航或重载。
  const second = await publish(request, headers, {
    ...doc,
    description: '另端发布的第二版',
    nodes: [{ id: 'new-gate', type: 'approval', approvers: ['alice'], instructions: '第二版审批' }],
  })
  expect(second.version).toBe(first.version + 1)
  const refresh = page.waitForResponse((res) => res.request().method() === 'GET' && new URL(res.url()).pathname === '/api/v1/definitions')
  await page.getByRole('button', { name: '刷新执行列表' }).click()
  expect((await refresh).ok()).toBe(true)
  await page.getByTestId(TID.triggerDefinition).click()
  await expect(page.getByRole('option', { name: `${doc.name} v${second.version}`, exact: true })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.triggerDefinition)).toContainText(`${doc.name} v${first.version}`)

  const posted: unknown[] = []
  page.on('request', (req) => {
    if (req.method() === 'POST' && new URL(req.url()).pathname === '/api/v1/executions') posted.push(req.postDataJSON())
  })
  const triggered = page.waitForResponse((res) => res.request().method() === 'POST' && new URL(res.url()).pathname === '/api/v1/executions')
  await page.getByTestId(TID.triggerSubmit).click()
  const response = await triggered
  expect(response.status()).toBe(201)
  const ack = await response.json() as { id: string; definition_version: number }
  expect(ack.definition_version).toBe(first.version)
  expect(posted).toEqual([{ definition_name: doc.name, version: first.version, input: {} }])
  await expect(page.getByTestId(tid.nodeRow('gate', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await expect(page.getByTestId(tid.nodeRow('new-gate', 0))).toHaveCount(0)
  const stored = await request.get(`/api/v1/executions/${ack.id}`, { headers })
  expect(stored.ok()).toBe(true)
  expect(await stored.json()).toMatchObject({ definition_id: first.id, definition_version: first.version, definition_hash: first.dsl_hash })

  // 缺省 version 的既有 API 仍解析到最新版,与 UI 的显式版本路径分别验收。
  const latest = await request.post('/api/v1/executions', {
    headers: { ...headers, 'Idempotency-Key': `t236-latest-${doc.name}` },
    data: { definition_name: doc.name, input: {} },
  })
  expect(latest.status()).toBe(201)
  expect(await latest.json()).toMatchObject({ definition_version: second.version })

  const archived = await request.post(`/api/v1/definitions/${doc.name}/versions/${first.version}/archive`, { headers })
  expect(archived.ok()).toBe(true)
  const before = await request.get(`/api/v1/executions?name=${doc.name}`, { headers })
  expect(before.ok()).toBe(true)
  const beforeIds = (await before.json() as { items: { id: string }[] }).items.map((item) => item.id)
  const refused = page.waitForResponse((res) => res.request().method() === 'POST' && new URL(res.url()).pathname === '/api/v1/executions')
  await page.getByTestId(TID.triggerSubmit).click()
  const rejection = await refused
  expect(rejection.status()).toBe(409)
  expect(await rejection.json()).toMatchObject({ error: 'conflict' })
  const error = page.getByTestId(TID.triggerPanel).getByRole('alert')
  await expect(error).toContainText(`${doc.name} v${first.version}`)
  await expect(error).toContainText('已归档或不可执行')
  await expect(error).toContainText('重新选择')
  expect(posted).toEqual([
    { definition_name: doc.name, version: first.version, input: {} },
    { definition_name: doc.name, version: first.version, input: {} },
  ])
  const after = await request.get(`/api/v1/executions?name=${doc.name}`, { headers })
  expect(after.ok()).toBe(true)
  expect((await after.json() as { items: { id: string }[] }).items.map((item) => item.id)).toEqual(beforeIds)
})
