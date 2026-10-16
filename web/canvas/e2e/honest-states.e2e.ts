// T23.8：真实 API/worker 验证缺参、缺模型、人工驳回；读取故障另用浏览器
// route 注入。故障注入只证明显示与退出行为，不冒充后端权限/网络验收。
import { expect, test, type APIRequestContext, type Page, type TestInfo } from '@playwright/test'
import { loadBase } from '../src/model/testdata.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }
const ALICE = { username: 'alice', password: 'e2e-alice-pass-1' }

async function login(page: Page, creds = AUTHOR) {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(creds.username)
  await page.getByTestId(TID.loginPassword).fill(creds.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
}

async function seed(request: APIRequestContext, suffix: string, approval = false) {
  const response = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(response.ok()).toBe(true)
  const { token } = await response.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const name = `t238-${suffix}-${Date.now()}`
  const doc = approval
    ? { dsl: '1.0', name, nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], mode: 'any', instructions: '请决定是否继续', outputs: { decision: 'string' } }], edges: [] }
    : { ...loadBase('minimal-llm'), name, inputs: [{ name: 'text', type: 'string', required: true }] }
  const created = await request.post('/api/v1/definitions', { headers, data: doc })
  expect(created.ok()).toBe(true)
  const { id } = await created.json() as { id: string }
  return { id, name, headers }
}

async function waitingExecution(request: APIRequestContext, suffix: string) {
  const draft = await seed(request, suffix, true)
  const published = await request.post(`/api/v1/drafts/${draft.id}/publish`, { headers: draft.headers })
  expect(published.ok()).toBe(true)
  const { version } = await published.json() as { version: number }
  const trigger = await request.post('/api/v1/executions', {
    headers: { ...draft.headers, 'Idempotency-Key': `${draft.name}-trigger` },
    data: { definition_name: draft.name, version, input: {} },
  })
  expect(trigger.ok()).toBe(true)
  return { ...draft, execId: (await trigger.json() as { id: string }).id }
}

async function screenshot(page: Page, info: TestInfo, name: string) {
  const file = info.outputPath(`${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
  await info.attach(name, { path: file, contentType: 'image/png' })
}

test('工作流列表断网不冒充空态，重试与保存后的刷新保留已有列表（路由故障注入）', async ({ page, request }, info) => {
  const { id } = await seed(request, 'draft-network')
  let offline = true
  await page.route('**/api/v1/definitions?*', async (route) => {
    if (new URL(route.request().url()).searchParams.get('status') === 'DRAFT' && offline) await route.abort('internetdisconnected')
    else await route.continue()
  })
  await login(page)
  await expect(page.getByTestId(TID.draftList).getByRole('alert')).toContainText('检查网络')
  await expect(page.getByTestId(TID.draftEmpty)).toHaveCount(0)
  await expect(page.getByText('工作流列表尚未读取成功')).toBeVisible()
  await screenshot(page, info, 'workflow-network-injected')
  offline = false
  await page.getByRole('button', { name: '重试工作流列表' }).click()
  await page.getByTestId(tid.draftItem(id)).click()
  await expect(page.getByTestId(TID.editor)).toBeVisible()
  offline = true
  await page.getByTestId(TID.editorSave).click()
  await expect(page.getByTestId(TID.draftList).getByRole('alert')).toContainText('保留上次读取')
  await expect(page.getByTestId(tid.draftItem(id))).toBeVisible()
  offline = false
  await page.getByRole('button', { name: '重试工作流列表' }).click()
  await expect(page.getByTestId(TID.draftList).getByRole('alert')).toHaveCount(0)
})

test('执行列表延迟与断网均不冒充空态，完整名称精确筛选可重试（路由故障注入）', async ({ page, request }, info) => {
  const { execId, name } = await waitingExecution(request, 'list')
  let release!: () => void
  const pending = new Promise<void>((resolve) => { release = resolve })
  let hold = true
  let offline = false
  await page.route('**/api/v1/executions?*', async (route) => {
    if (new URL(route.request().url()).searchParams.get('limit') !== '20') { await route.continue(); return }
    if (hold) await pending
    if (offline) await route.abort('internetdisconnected')
    else await route.continue()
  })
  await login(page)
  await page.getByTestId(TID.navExecutions).click()
  await expect(page.getByRole('region', { name: '执行列表' })).toHaveAttribute('aria-busy', 'true')
  await expect(page.getByText('正在加载执行列表…')).toBeVisible()
  await expect(page.getByTestId(TID.execEmpty)).toHaveCount(0)
  hold = false
  release()
  await expect(page.getByTestId(tid.execRow(execId))).toBeVisible()
  await expect(page.getByText('精确匹配，须输入完整名称')).toBeVisible()
  const filtered = page.waitForResponse((res) => new URL(res.url()).pathname === '/api/v1/executions' && new URL(res.url()).searchParams.get('name') === name)
  await page.getByLabel('按名称', { exact: true }).fill(name)
  expect((await filtered).ok()).toBe(true)
  await expect(page.getByTestId(tid.execRow(execId))).toBeVisible()
  offline = true
  await page.getByRole('button', { name: '刷新执行列表', exact: true }).click()
  await expect(page.getByRole('region', { name: '执行列表' }).getByRole('alert')).toContainText('检查网络')
  await expect(page.getByTestId(TID.execEmpty)).toHaveCount(0)
  await screenshot(page, info, 'execution-list-network-injected')
  offline = false
  await page.getByRole('button', { name: '重试执行列表' }).click()
  await expect(page.getByTestId(tid.execRow(execId))).toBeVisible()
})

for (const failure of ['offline', '403', '404'] as const) {
  test(`详情 ${failure} 读取失败暂停轮询，有界重试与返回（路由故障注入）`, async ({ page, request }, info) => {
    const { execId } = await waitingExecution(request, `read-${failure}`)
    let failed = true
    let reads = 0
    await page.route(`**/api/v1/executions/${execId}`, async (route) => {
      reads++
      if (!failed) { await route.continue(); return }
      if (failure === 'offline') await route.abort('internetdisconnected')
      else await route.fulfill({ status: Number(failure), json: { error: failure === '404' ? 'not_found' : 'role_denied' } })
    })
    await login(page)
    await page.goto(`/executions/${execId}`)
    const detail = page.getByTestId(TID.execDetail)
    await expect(detail.getByRole('alert')).toContainText(failure === 'offline' ? '检查网络' : failure === '403' ? '角色权限' : '目标不存在')
    await expect(detail.getByRole('button', { name: '返回执行列表' })).toBeVisible()
    const before = reads
    // 超过两个既有 2.5s 轮询窗；错误面不能持续请求。
    await page.waitForTimeout(5500)
    expect(reads).toBe(before)
    await screenshot(page, info, `execution-${failure}-injected`)
    failed = false
    await detail.getByRole('button', { name: '重试读取执行' }).click()
    await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', /PENDING|RUNNING|WAITING_APPROVAL/)
    expect(reads).toBe(before + 1)
    failed = true
    await detail.getByRole('button', { name: '刷新', exact: true }).click()
    await expect(detail.getByRole('alert')).toContainText('自动刷新已暂停')
    await expect(page.getByTestId(TID.resumeSubmit)).toBeDisabled()
    // 已有内容刷新失败仍保留身份与原因，初读失败的返回出口也有真实路由验证。
    await page.goto(`/executions/${execId}`)
    await detail.getByRole('button', { name: '返回执行列表' }).click()
    await expect(page).toHaveURL(/\/executions$/)
  })
}

test('缺模型在编辑和真实worker失败中均可辨；缺参数先就地说明（真实后端，零供应商调用）', async ({ page, request }, info) => {
  const { id, name, headers } = await seed(request, 'missing-model')
  await login(page)
  await page.goto(`/workflows/${id}`)
  await expect(page.getByTestId(tid.nodeCard('only'))).toContainText('模型未配置')
  await expect(page.getByText(/草稿可保存；发布前请补齐/)).toBeVisible()
  await page.getByTestId(tid.nodeCard('only')).click()
  await expect(page.getByLabel('模型', { exact: true })).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByText('模型未配置，执行此节点会失败。')).toBeVisible()
  await page.getByTestId(TID.editorPublish).click()
  await expect(page.getByTestId(TID.workflowFlash)).toContainText(name)
  await page.getByTestId(TID.viewPublished).click()
  await page.getByTestId(TID.triggerDefinition).click()
  await page.getByRole('option', { name: `${name} v1`, exact: true }).click()
  let posts = 0
  page.on('request', (req) => { if (req.method() === 'POST' && new URL(req.url()).pathname === '/api/v1/executions') posts++ })
  await page.getByTestId(TID.triggerSubmit).click()
  await expect(page.getByRole('alert')).toContainText('缺少必填参数：text')
  expect(posts).toBe(0)
  await page.getByTestId(TID.triggerInput).fill('{"text":"missing model proof"}')
  const response = page.waitForResponse((res) => res.request().method() === 'POST' && new URL(res.url()).pathname === '/api/v1/executions')
  await page.getByTestId(TID.triggerSubmit).click()
  const { id: execId } = await (await response).json() as { id: string }
  await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  await expect(page.getByTestId(TID.execDetail).getByRole('alert')).toContainText('缺少模型名称或消息')
  await screenshot(page, info, 'missing-model-real-worker')
  const stored = await request.get(`/api/v1/executions/${execId}`, { headers })
  const execution = await stored.json() as { error_code: string; nodes: { error_code: string; error_message: string }[] }
  expect(execution.error_code).toBe('node_failed')
  expect(execution.nodes[0]).toMatchObject({ error_code: 'llm_call_failed', error_message: 'llm: request needs model and messages' })
})

test('人工驳回按真实审批链显示中性原因，保留服务端FAILED与原始证据（真实后端）', async ({ page, request }, info) => {
  const { execId, headers } = await waitingExecution(request, 'reject')
  await login(page, ALICE)
  await page.goto(`/approvals/${execId}/gate`)
  await expect(page.getByTestId(TID.approvalReject)).toBeVisible({ timeout: 30_000 })
  await page.getByTestId(TID.approvalComment).fill('T23.8 真实人工驳回证据')
  await page.getByTestId(TID.approvalReject).click()
  await page.getByRole('button', { name: '确认驳回', exact: true }).click()
  await expect(page.getByTestId(TID.approvalFlash)).toBeVisible()
  await page.goto(`/executions/${execId}`)
  await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  const rejected = page.locator('.alert[data-tone="neutral"]').filter({ has: page.getByText('审批驳回', { exact: true }) })
  await expect(rejected).toContainText('人工审批已驳回')
  await expect(page.getByText('执行错误', { exact: true })).toHaveCount(0)
  await rejected.getByText('原始错误', { exact: true }).click()
  await expect(rejected).toContainText('approval_rejected')
  await screenshot(page, info, 'approval-rejected-real-backend')
  const stored = await request.get(`/api/v1/executions/${execId}`, { headers })
  expect(await stored.json()).toMatchObject({ status: 'FAILED', error_code: 'node_failed', nodes: [{ type: 'approval', status: 'FAILED', error_code: 'approval_rejected' }] })
})
