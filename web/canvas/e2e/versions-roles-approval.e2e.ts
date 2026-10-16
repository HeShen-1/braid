// T23.10真实登录/版本/审批旅程：只使用独立e2e栈的author与alice，零LLM调用。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }
const ALICE = { username: 'alice', password: 'e2e-alice-pass-1' }

async function apiAuthor(request: APIRequestContext) {
  const response = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(response.ok()).toBe(true)
  const login = await response.json() as { token: string; username: string; roles: string[]; expires_at: string }
  expect(login.username).toBe('author')
  expect(login.roles).toEqual(['author'])
  return { Authorization: `Bearer ${login.token}` }
}
async function login(page: Page, creds = AUTHOR) {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/login')
  await page.getByTestId(TID.loginUsername).fill(creds.username)
  await page.getByTestId(TID.loginPassword).fill(creds.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.whoami)).toContainText(creds.username)
}
async function publish(request: APIRequestContext, headers: Record<string, string>, name: string, instructions: string) {
  const created = await request.post('/api/v1/definitions', { headers, data: { dsl: '1.0', name, inputs: [{ name: 'text', type: 'string', required: true }], nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], instructions, outputs: { decision: 'string' } }], edges: [] } })
  expect(created.ok()).toBe(true)
  const id = (await created.json() as { id: string }).id
  const response = await request.post(`/api/v1/drafts/${id}/publish`, { headers })
  expect(response.ok()).toBe(true)
  return await response.json() as { version: number }
}
async function trigger(request: APIRequestContext, headers: Record<string, string>, name: string, version: number) {
  const response = await request.post('/api/v1/executions', { headers: { ...headers, 'Idempotency-Key': `${name}-${version}-${Date.now()}` }, data: { definition_name: name, version, input: { text: '审批需阅读的原始输入' } } })
  expect(response.ok()).toBe(true)
  const id = (await response.json() as { id: string }).id
  await expect.poll(async () => {
    const read = await request.get(`/api/v1/executions/${id}`, { headers })
    expect(read.ok()).toBe(true)
    return (await read.json() as { nodes: { node_id: string; status: string }[] }).nodes.find((n) => n.node_id === 'gate')?.status
  }, { timeout: 30_000 }).toBe('WAITING_APPROVAL')
  return id
}

test('author查两个版本/定义并归档，全部归档后刷新仍能回看', async ({ page, request }, info) => {
  const headers = await apiAuthor(request)
  const name = `t2310-versions-${Date.now()}`
  const first = await publish(request, headers, name, '版本一独有说明')
  const second = await publish(request, headers, name, '版本二独有说明')
  await login(page)
  await expect(page.getByTestId(TID.whoami)).toContainText('author')
  await page.getByRole('button', { name: '已发布与版本历史', exact: true }).click()
  await page.getByRole('combobox', { name: '工作流', exact: true }).click()
  await page.getByRole('option', { name, exact: true }).click()
  await page.getByRole('button', { name: `查看 v${first.version} 定义`, exact: true }).click()
  await expect(page.getByLabel('版本定义')).toContainText('版本一独有说明')
  for (const version of [first.version, second.version]) {
    await page.getByRole('button', { name: `归档 v${version}`, exact: true }).click()
    await expect(page.getByRole('group', { name: `归档 ${name} v${version}`, exact: true })).toBeFocused()
    await page.keyboard.press('Escape')
    await expect(page.getByRole('button', { name: `归档 v${version}`, exact: true })).toBeFocused()
    await page.getByRole('button', { name: `归档 v${version}`, exact: true }).click()
    await page.getByRole('button', { name: `确认归档 v${version}`, exact: true }).click()
    await expect(page.getByRole('button', { name: `归档 v${version}`, exact: true })).toHaveCount(0)
  }
  await page.reload()
  await page.getByRole('combobox', { name: '工作流', exact: true }).click()
  await page.getByRole('option', { name, exact: true }).click()
  await page.getByRole('button', { name: `查看 v${second.version} 定义`, exact: true }).click()
  await expect(page.getByLabel('版本定义')).toContainText('版本二独有说明')
  await expect(page.getByLabel('版本定义')).toContainText('已归档')
  await page.screenshot({ path: info.outputPath('versions-archived.png'), fullPage: true, animations: 'disabled' })
})

for (const decision of ['approve', 'reject'] as const) {
  test(`approver只读编排/执行，新待办自动出现，${decision} 后可追全文与执行`, async ({ page, request }, info) => {
    const headers = await apiAuthor(request)
    const name = `t2310-${decision}-${Date.now()}`
    const version = await publish(request, headers, name, '请阅读输入后作决定')
    await login(page, ALICE)
    await expect(page.getByTestId(TID.whoami)).toContainText('approver')
    await expect(page.getByTestId(TID.draftNew)).toHaveCount(0)
    await expect(page.getByTestId(TID.editor)).toHaveCount(0)
    await expect(page.getByRole('button', { name: /^归档 v/ })).toHaveCount(0)
    await page.getByTestId(TID.navExecutions).click()
    await expect(page.getByTestId(TID.triggerSubmit)).toHaveCount(0)
    await page.getByTestId(TID.navApprovals).click()
    await expect(page.getByTestId(TID.approvalQueue)).toBeVisible()
    const id = await trigger(request, headers, name, version.version)
    const row = page.getByTestId(tid.approvalRow(id, 'gate'))
    await expect(row).toBeVisible({ timeout: 15_000 }) // 等真实5秒轮询，不点刷新
    await row.focus()
    await page.keyboard.press('Enter')
    await expect(page.getByTestId(TID.approvalApprove)).toBeVisible()
    await expect(page.getByText('触发人：')).toContainText('author')
    await expect(page.getByText('审批需阅读的原始输入')).toBeVisible()
    const comment = `T23.10 ${decision} 的完整决定\n第二行 <script>只能是文本</script>`
    await page.getByTestId(TID.approvalComment).fill(comment)
    await page.getByRole('button', { name: '返回队列', exact: true }).first().focus()
    if (decision === 'approve') await page.keyboard.press('a')
    else { await page.keyboard.press('r'); await page.getByRole('button', { name: '确认驳回', exact: true }).click() }
    await expect(page.getByTestId(TID.approvalQueue)).toBeVisible()
    await expect.poll(() => page.evaluate(() => Boolean(document.activeElement?.closest('[aria-label="待办审批队列"]')))).toBe(true)
    await page.getByRole('button', { name: '查看刚才的决定', exact: true }).click()
    await expect(page.getByTestId(TID.approvalTimeline)).toContainText(comment)
    await expect(page.getByTestId(TID.approvalTimeline)).toContainText('alice')
    await expect(page.getByTestId(TID.approvalApprove)).toHaveCount(0)
    await page.getByRole('link', { name: '查看执行详情与审计', exact: true }).click()
    const panel = page.getByTestId(tid.approvalEntry('gate'))
    await expect(panel).toContainText(comment)
    await expect(page.getByTestId(TID.rollbackSubmit)).toHaveCount(0)
    await expect(panel.getByRole('link', { name: '查看决定历史', exact: true })).toBeVisible()
    const denied = await request.get(`/api/v1/executions/${id}/approvals/gate`, { headers })
    expect(denied.status()).toBe(403) // author不在审批名单，后端边界继续成立
    await info.attach('real-approval-context', { body: JSON.stringify({ executionId: id, decision, comment, authorApprovalReadStatus: denied.status() }), contentType: 'application/json' })
    await page.screenshot({ path: info.outputPath(`decision-${decision}.png`), fullPage: true, animations: 'disabled' })
  })
}
