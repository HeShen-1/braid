// T16.6 浏览器级回滚-重跑 e2e(D-2026-10-04-t166-playwright-e2e)。
// 旅程:作者经画布触发 → alice 审批台驳回门一(FAILED)→ 画布回滚到
// gate1 → alice 二轮批准 → bob 驳回门二(二次 FAILED)→ 画布续跑(自动
// 定位)→ bob 二轮批准 → file_read 并行扇出(实例行渲染)→ 会签中间态
// (alice 画布视角:待投名单收缩)→ bob 终票 → SUCCEEDED + 审计链完整
// (execution.rolled_back ×2 / approval.decided ×6 / node.state_changed)。
// 断言全部走 UI(浏览器版口径,webjourney Go 面的浏览器回收);失败注入
// = 审批驳回(确定性,零 mock server);栈 = deploy/e2e-up.sh 四进程。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import path from 'node:path'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }
const ALICE = { username: 'alice', password: 'e2e-alice-pass-1' }
const BOB = { username: 'bob', password: 'e2e-bob-pass-1' }

const FIXTURE_DSL = readFileSync(
  path.resolve(process.cwd(), 'e2e/fixtures/rollback-journey.json'),
  'utf8',
)
const DOC_FILES = ['a.txt', 'b.txt'].map((f) =>
  path.resolve(process.cwd(), 'e2e/fixtures/files', f),
)

/** 发布 fixture(每次运行发布新版本;触发走最新)。 */
async function publishFixture(req: APIRequestContext): Promise<void> {
  const login = await req.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok(), 'author login').toBeTruthy()
  const { token } = (await login.json()) as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const created = await req.post('/api/v1/definitions', { headers, data: FIXTURE_DSL })
  expect(created.status(), 'create draft').toBe(201)
  const { id } = (await created.json()) as { id: string }
  const published = await req.post(`/api/v1/drafts/${id}/publish`, { headers })
  expect(published.ok(), 'publish draft').toBeTruthy()
}

async function canvasLogin(page: Page, creds: { username: string; password: string }): Promise<void> {
  await page.goto('/ui/canvas/')
  await page.getByLabel('用户名').fill(creds.username)
  await page.getByLabel('密码').fill(creds.password)
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('nav-executions')).toBeVisible()
}

async function consoleLogin(page: Page, creds: { username: string; password: string }): Promise<void> {
  await page.goto('/ui/')
  await page.locator('#username').fill(creds.username)
  await page.locator('#password').fill(creds.password)
  await page.locator('#login-form button[type=submit]').click()
  // 空待办时 #todo-body 是 hidden 空表——断列表视图本体。
  await expect(page.locator('#list-view')).toBeVisible()
}

/** 审批台决定:回到待办列表(token 已在 sessionStorage)→ 行内「处理」→ 批准/驳回。 */
async function consoleDecide(
  page: Page,
  nodeId: string,
  execId: string,
  decision: 'approve' | 'reject',
): Promise<void> {
  await page.goto('/ui/')
  const row = page
    .locator('#todo-body tr')
    .filter({ hasText: execId })
    .filter({ hasText: nodeId })
  await expect(row.first()).toBeVisible({ timeout: 30_000 })
  await row.first().locator('button').click()
  await expect(page.locator('#decision-form')).toBeVisible()
  await page.locator(decision === 'approve' ? '#approve' : '#reject').click()
  // 决定成功 = alert + 回待办列表(alert 由 dialog 监听收掉);唯一的待办
  // 被消化后列表为空(空表 hidden),断视图本体不断表体。
  await expect(page.locator('#list-view')).toBeVisible({ timeout: 15_000 })
}

/** 从详情头部 `#<id> · name vX` 解出执行 id(头部 .panel-id 是首个;审计区同类名)。 */
async function currentExecId(page: Page): Promise<string> {
  const text = await page.getByTestId('exec-detail').locator('.panel-id').first().innerText()
  return /^#(\d+)/.exec(text.trim())?.[1] ?? ''
}

test('回滚-重跑全旅程:驳回翻活 → 并行实例 → 会签中间态 → 终态 + 审计链', async ({
  browser,
  request,
}) => {
  test.setTimeout(300_000)
  await publishFixture(request)

  const authorCtx = await browser.newContext()
  const aliceCtx = await browser.newContext()
  const bobCtx = await browser.newContext()
  const author = await authorCtx.newPage()
  const alice = await aliceCtx.newPage()
  const bob = await bobCtx.newPage()
  for (const p of [alice, bob]) p.on('dialog', (d) => void d.accept())

  // ① 作者触发(画布 UI:定义下拉 + 输入 JSON,幂等键由 UI 生成)。
  await canvasLogin(author, AUTHOR)
  await author.getByTestId('nav-executions').click()
  await expect(author.getByTestId('trigger-panel')).toBeVisible()
  await author.getByTestId('trigger-definition').selectOption('e2e-rollback-journey')
  await author.getByTestId('trigger-input').fill(JSON.stringify({ docs: DOC_FILES }))
  await author.getByTestId('trigger-submit').click()
  await expect(author.getByTestId('exec-detail')).toBeVisible({ timeout: 15_000 })
  const execId = await currentExecId(author)
  expect(execId).not.toBe('')

  // ② 门一挂起;作者非审批人 → 画布无 approval 子面板(名单内可见性,
  // GET approvals/:node 对名单外 403 not_approver → 静默收起)。
  await expect(author.getByTestId('node-row-gate1-0')).toContainText('WAITING_APPROVAL', { timeout: 30_000 })
  await expect(author.getByTestId('approval-gate1')).toHaveCount(0)

  // ③ alice 驳回门一 → 执行 FAILED(approval_rejected,fail-fast)。
  await consoleLogin(alice, ALICE)
  await consoleDecide(alice, 'gate1', execId, 'reject')
  await expect(author.getByTestId('exec-status')).toHaveText('FAILED', { timeout: 30_000 })
  await expect(author.getByTestId('node-row-gate1-0')).toContainText('approval_rejected')

  // ④ 画布回滚:选 gate1 行 → 回滚 → 重挂起(round 2)。
  await author.getByTestId('node-row-gate1-0').click()
  await author.getByTestId('rollback-submit').click()
  await expect(author.getByTestId('exec-flash')).toBeVisible()
  await expect(author.getByTestId('node-row-gate1-0')).toContainText('WAITING_APPROVAL', { timeout: 30_000 })

  // ⑤ alice 二轮批准 → 门二挂起 → bob 驳回(第二次 FAILED)。
  await consoleDecide(alice, 'gate1', execId, 'approve')
  await expect(author.getByTestId('node-row-gate2-0')).toContainText('WAITING_APPROVAL', { timeout: 30_000 })
  await consoleLogin(bob, BOB)
  await consoleDecide(bob, 'gate2', execId, 'reject')

  // ⑥ 画布续跑(无选点,自动定位首个 FAILED)→ 门二重挂起 → bob 批准。
  await expect(author.getByTestId('exec-status')).toHaveText('FAILED', { timeout: 30_000 })
  await author.getByTestId('resume-submit').click()
  await expect(author.getByTestId('exec-flash')).toBeVisible()
  await expect(author.getByTestId('node-row-gate2-0')).toContainText('WAITING_APPROVAL', { timeout: 30_000 })
  await consoleDecide(bob, 'gate2', execId, 'approve')

  // ⑦ 并行扇出:file_read 实例行 1..2 各自 SUCCEEDED(t14.2 字段契约渲染)。
  await expect(author.getByTestId('node-row-fan-1')).toContainText('SUCCEEDED', { timeout: 30_000 })
  await expect(author.getByTestId('node-row-fan-2')).toContainText('SUCCEEDED', { timeout: 30_000 })

  // ⑧ 会签中间态(alice 画布视角,名单内审批人可见子资源):
  //    待投名单全量 → alice 投票后收缩到 bob。
  await expect(author.getByTestId('node-row-sign-0')).toContainText('WAITING_APPROVAL', { timeout: 30_000 })
  const aliceCanvas = await aliceCtx.newPage()
  await canvasLogin(aliceCanvas, ALICE)
  await aliceCanvas.getByTestId('nav-executions').click()
  await aliceCanvas.getByTestId(`exec-row-${execId}`).click()
  await expect(aliceCanvas.getByTestId('exec-detail')).toBeVisible({ timeout: 15_000 })
  await expect(aliceCanvas.getByTestId('approval-sign')).toBeVisible({ timeout: 15_000 })
  await expect(aliceCanvas.getByTestId('approval-sign')).toContainText('会签')
  await expect(aliceCanvas.getByTestId('pending-approvers')).toHaveText('alice, bob')
  await consoleDecide(alice, 'sign', execId, 'approve')
  await expect(aliceCanvas.getByTestId('pending-approvers')).toHaveText('bob', { timeout: 20_000 })

  // ⑨ bob 终票 → 终态 + 输出 + 审计链(reaper flush 后 node 级事件到场)。
  await consoleDecide(bob, 'sign', execId, 'approve')
  await expect(author.getByTestId('exec-status')).toHaveText('SUCCEEDED', { timeout: 30_000 })
  await expect(author.getByTestId('exec-outputs')).toContainText('approved')

  await expect(author.getByTestId('audit-row-execution.triggered')).toBeVisible({ timeout: 20_000 })
  expect(await author.getByTestId('audit-row-execution.rolled_back').count()).toBeGreaterThanOrEqual(2)
  expect(await author.getByTestId('audit-row-approval.decided').count()).toBeGreaterThanOrEqual(6)
  expect(await author.getByTestId('audit-row-node.state_changed').count()).toBeGreaterThanOrEqual(1)

  await authorCtx.close()
  await aliceCtx.close()
  await bobCtx.close()
})
