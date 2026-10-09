// T16.6 浏览器级回滚-重跑 e2e(D-2026-10-04-t166-playwright-e2e)。
// 旅程:作者经控制台触发 → alice 审批台驳回门一(FAILED)→ 控制台回滚到
// gate1 → alice 二轮批准 → bob 驳回门二(二次 FAILED)→ 控制台续跑(自动
// 定位)→ bob 二轮批准 → file_read 并行扇出(实例行渲染)→ 会签中间态
// (alice 控制台视角:待投名单收缩)→ bob 终票 → SUCCEEDED + 审计链完整
// (execution.rolled_back ×2 / approval.decided ×6 / node.state_changed)。
// 断言全部走 UI(浏览器版口径,webjourney Go 面的浏览器回收);失败注入
// = 审批驳回(确定性,零 mock server);栈 = deploy/e2e-up.sh 四进程。
//
// T21.1(D-2026-10-09-ui-console):
//   - 选择器单一真源 = src/ui/testids.ts(直接 import TID/tid,禁字面量);
//   - 本 PR 路由仍在 /ui/canvas/(控制台,react-router basename)与 /ui/
//     (审批台旧三件套,T21.2 才并入),goto 路径不提前切 /workflows;
//   - 执行 id 从触发 POST 响应回读(TriggerAck.id),不依赖详情头部 DOM;
//   - SPA 回退(非资源路径 → index.html)在浏览器面单独一测兜底。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { TID, tid } from '../src/ui/testids.ts'

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

/** 控制台登录(SPA 单一登录面;表单 testid 见 testids.ts)。 */
async function canvasLogin(page: Page, creds: { username: string; password: string }): Promise<void> {
  await page.goto('/ui/canvas/')
  await page.getByTestId(TID.loginUsername).fill(creds.username)
  await page.getByTestId(TID.loginPassword).fill(creds.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
}

/** 审批台登录(/ui/ 旧三件套,T21.2 前形态;选择器属该面自身契约)。 */
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

/** 触发面板提交一次执行,并从 TriggerAck 回读执行 id(幂等键由 UI 生成)。 */
async function triggerFromConsole(page: Page): Promise<string> {
  await expect(page.getByTestId(TID.triggerPanel)).toBeVisible()
  await page.getByTestId(TID.triggerDefinition).selectOption('e2e-rollback-journey')
  await page.getByTestId(TID.triggerInput).fill(JSON.stringify({ docs: DOC_FILES }))
  const [res] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith('/api/v1/executions'),
    ),
    page.getByTestId(TID.triggerSubmit).click(),
  ])
  expect(res.status(), 'trigger execution').toBeLessThan(300)
  const ack = (await res.json()) as { id: string }
  expect(ack.id, 'TriggerAck.id').toBeTruthy()
  return ack.id
}

// T21.1 Go 面契约的浏览器兑现(决策 6):客户端深链由 index.html 启动
// (SPA 回退 200 + react-router 接管),资源缺失不被回退吞成 HTML。
test('SPA 回退:控制台深链直达可启动,资源缺失仍 404', async ({ page, request }) => {
  // 深链未登录 → 应用启动后由 RequireAuth 收编到登录页(Go 必须 200 而非 404)。
  for (const deepLink of ['/ui/canvas/executions', '/ui/canvas/workflows']) {
    const res = await page.goto(deepLink)
    expect(res?.status(), `${deepLink} 状态`).toBe(200)
    await expect(page.getByTestId(TID.loginUsername)).toBeVisible()
  }
  // 资源后缀与禁目录列表不回退(noDirListing 契约)。
  expect((await request.get('/ui/canvas/nope.js')).status()).toBe(404)
  expect((await request.get('/ui/canvas/assets/')).status()).toBe(404)
})

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

  // ① 作者触发(控制台:定义下拉 + 输入 JSON,幂等键由 UI 生成)。
  await canvasLogin(author, AUTHOR)
  await author.getByTestId(TID.navExecutions).click()
  const execId = await triggerFromConsole(author)
  await expect(author.getByTestId(TID.execDetail)).toBeVisible({ timeout: 15_000 })

  // ② 门一挂起;作者非审批人 → 控制台无 approval 子面板(名单内可见性,
  // GET approvals/:node 对名单外 403 not_approver → 静默收起)。
  // 状态断 data-state 原始 ENUM(可见文本是中文标签,状态色相 + 形状双
  // 编码,文案随设计系统走;ENUM 才是机器可断言的稳定契约)。
  await expect(author.getByTestId(tid.nodeRow('gate1', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await expect(author.getByTestId(tid.approvalEntry('gate1'))).toHaveCount(0)

  // ③ alice 驳回门一 → 执行 FAILED(approval_rejected,fail-fast)。
  await consoleLogin(alice, ALICE)
  await consoleDecide(alice, 'gate1', execId, 'reject')
  await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  await expect(author.getByTestId(tid.nodeRow('gate1', 0))).toContainText('approval_rejected')

  // ④ 控制台回滚:选 gate1 行 → 回滚 → 重挂起(round 2)。
  await author.getByTestId(tid.nodeRow('gate1', 0)).click()
  await author.getByTestId(TID.rollbackSubmit).click()
  await expect(author.getByTestId(TID.execFlash)).toBeVisible()
  await expect(author.getByTestId(tid.nodeRow('gate1', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })

  // ⑤ alice 二轮批准 → 门二挂起 → bob 驳回(第二次 FAILED)。
  await consoleDecide(alice, 'gate1', execId, 'approve')
  await expect(author.getByTestId(tid.nodeRow('gate2', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await consoleLogin(bob, BOB)
  await consoleDecide(bob, 'gate2', execId, 'reject')

  // ⑥ 控制台续跑(无选点,自动定位首个 FAILED)→ 门二重挂起 → bob 批准。
  await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  await author.getByTestId(TID.resumeSubmit).click()
  await expect(author.getByTestId(TID.execFlash)).toBeVisible()
  await expect(author.getByTestId(tid.nodeRow('gate2', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await consoleDecide(bob, 'gate2', execId, 'approve')

  // ⑦ 并行扇出:file_read 实例行 1..2 各自 SUCCEEDED(t14.2 字段契约渲染)。
  await expect(author.getByTestId(tid.nodeRow('fan', 1))).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 30_000 })
  await expect(author.getByTestId(tid.nodeRow('fan', 2))).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 30_000 })

  // ⑧ 会签中间态(alice 控制台视角,名单内审批人可见子资源):
  //    待投名单全量 → alice 投票后收缩到 bob。
  await expect(author.getByTestId(tid.nodeRow('sign', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  const aliceCanvas = await aliceCtx.newPage()
  await canvasLogin(aliceCanvas, ALICE)
  await aliceCanvas.getByTestId(TID.navExecutions).click()
  await aliceCanvas.getByTestId(tid.execRow(execId)).click()
  await expect(aliceCanvas.getByTestId(TID.execDetail)).toBeVisible({ timeout: 15_000 })
  await expect(aliceCanvas.getByTestId(tid.approvalEntry('sign'))).toBeVisible({ timeout: 15_000 })
  await expect(aliceCanvas.getByTestId(tid.approvalEntry('sign'))).toContainText('会签')
  await expect(aliceCanvas.getByTestId(TID.pendingApprovers)).toHaveText('alice, bob')
  await consoleDecide(alice, 'sign', execId, 'approve')
  await expect(aliceCanvas.getByTestId(TID.pendingApprovers)).toHaveText('bob', { timeout: 20_000 })

  // ⑨ bob 终票 → 终态 + 输出 + 审计链(reaper flush 后 node 级事件到场)。
  await consoleDecide(bob, 'sign', execId, 'approve')
  await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 30_000 })
  await expect(author.getByTestId(TID.execOutputs)).toContainText('approved')

  await expect(author.getByTestId(tid.auditRow('execution.triggered'))).toBeVisible({ timeout: 20_000 })
  expect(await author.getByTestId(tid.auditRow('execution.rolled_back')).count()).toBeGreaterThanOrEqual(2)
  expect(await author.getByTestId(tid.auditRow('approval.decided')).count()).toBeGreaterThanOrEqual(6)
  expect(await author.getByTestId(tid.auditRow('node.state_changed')).count()).toBeGreaterThanOrEqual(1)

  await authorCtx.close()
  await aliceCtx.close()
  await bobCtx.close()
})
