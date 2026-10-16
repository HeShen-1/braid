// T16.6 浏览器级回滚-重跑 e2e(D-2026-10-04-t166-playwright-e2e)。
// 旅程:作者经控制台触发 → alice 审批台驳回门一(FAILED)→ 控制台回滚到
// gate1 → alice 二轮批准 → bob 驳回门二(二次 FAILED)→ 控制台续跑(自动
// 定位)→ bob 二轮批准 → file_read 并行扇出(实例行渲染)→ 会签中间态
// (alice 控制台视角:待投名单收缩)→ bob 终票 → SUCCEEDED + 审计链完整
// (execution.rolled_back ×2 / approval.decided ×6 / node.state_changed)。
// 断言全部走 UI(浏览器版口径,webjourney Go 面的浏览器回收);失败注入
// = 审批驳回(确定性,零 mock server);栈 = deploy/e2e-up.sh 四进程。
//
// T21.2(D-2026-10-09-ui-console 决策 6,路由硬切):
//   - 选择器单一真源 = src/ui/testids.ts(直接 import TID/tid,禁字面量);
//   - 路径:控制台/执行面 = 根路径 `/`(→ /workflows/)与 /executions/;
//     审批台 = /approvals/——原审批台原生三件套已并入同一 SPA,旧审批台/
//     画布前缀硬切 404(本文件单独一测钉死,无 302);
//   - 登录面统一:控制台与审批台共用 login-username/login-password/login-submit;
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

/** 发布 fixture(每次运行发布新版本;界面选择该已发布版本再触发)。 */
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

/** 控制台/执行面登录(SPA 单一登录面;表单 testid 见 testids.ts)。 */
async function canvasLogin(page: Page, creds: { username: string; password: string }): Promise<void> {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(creds.username)
  await page.getByTestId(TID.loginPassword).fill(creds.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
}

/** 审批台登录(/approvals/,与画布同一 SPA 同一登录面)。 */
async function approvalLogin(page: Page, creds: { username: string; password: string }): Promise<void> {
  await page.goto('/approvals/')
  await page.getByTestId(TID.loginUsername).fill(creds.username)
  await page.getByTestId(TID.loginPassword).fill(creds.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.approvalQueue)).toBeVisible({ timeout: 15_000 })
}

/** 审批台决定:回待办队列(token 已在 sessionStorage)→ 行 → 详情 → 批准/驳回。
 *  驳回是行内确认层(会签驳回即判死):approvalReject 单击只布防,「确认驳回」
 *  才落 POST。决定成功 = 回队列 + approvalFlash(原原生三件套的 alert 已退役)。 */
async function approvalDecide(
  page: Page,
  nodeId: string,
  execId: string,
  decision: 'approve' | 'reject',
): Promise<void> {
  await page.goto('/approvals')
  const row = page.getByTestId(tid.approvalRow(execId, nodeId))
  await expect(row).toBeVisible({ timeout: 30_000 })
  await row.click()
  // 审批面状态同走 data-state 原始 ENUM(详情根 main 带 node_status,与执行面
  // node 行同语言;不读中文可见文本——T21.1 收尾口径)。
  const detail = page.getByTestId(TID.approvalDetail)
  await expect(detail).toBeVisible()
  await expect(detail).toHaveAttribute('data-state', 'WAITING_APPROVAL')
  // 决定表单在场 = 可操作(收口节点整块不在 DOM,不是 disabled)。
  await expect(page.getByTestId(TID.approvalComment)).toBeVisible()
  if (decision === 'approve') {
    await page.getByTestId(TID.approvalApprove).click()
  } else {
    await page.getByTestId(TID.approvalReject).click()
    await page.getByRole('button', { name: '确认驳回' }).click()
  }
  await expect(page.getByTestId(TID.approvalFlash)).toBeVisible({ timeout: 30_000 })
  await expect(page.getByTestId(TID.approvalQueue)).toBeVisible()
}

/** 触发面板提交一次执行,并从 TriggerAck 回读执行 id(幂等键由 UI 生成)。 */
async function triggerFromConsole(page: Page): Promise<string> {
  await expect(page.getByTestId(TID.triggerPanel)).toBeVisible()
  // T21.3 决策 3 收敛后 Select 不再是原生控件:两步 = 点触发器 → 点选项。
  // 选项文案 = `${name} v${version}`,初次仅列已知最高版本;触发请求携带该版本。
  await page.getByTestId(TID.triggerDefinition).click()
  await page.getByRole('option', { name: /^e2e-rollback-journey v/ }).first().click()
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

// T21.2 Go 面契约的浏览器兑现(决策 6):客户端深链由 index.html 启动
// (SPA 回退 200 + react-router 接管),旧审批台/画布前缀无重定向一律 404,
// 资源缺失不被回退吞成 HTML。
test('SPA 回退与硬切:根路径/三条深链直达可启动,旧前缀与资源缺失 404', async ({
  page,
  request,
}) => {
  // 深链未登录 → 应用启动后由 RequireAuth 收编到登录页(Go 必须 200 而非 404,
  // 且零 Location 头——FileServer 的 /index.html 301 归一化不得回流到深链)。
  for (const deepLink of ['/', '/workflows', '/executions', '/approvals']) {
    const res = await page.goto(deepLink)
    expect(res?.status(), `${deepLink} 状态`).toBe(200)
    expect(res?.headers()['location'], `${deepLink} 不应重定向`).toBeUndefined()
    await expect(page.getByTestId(TID.loginUsername)).toBeVisible()
  }
  // 路由硬切:旧面不注册任何路由 = 404,不留 302(maxRedirects:0 让任何
  // 3xx 都落进断言,而非被跟随成 200)。
  for (const dead of ['/ui', '/ui/', '/ui/canvas/', '/ui/app.js']) {
    const res = await request.get(dead, { maxRedirects: 0 })
    expect(res.status(), `${dead} 必须 404`).toBe(404)
  }
  // 浏览器面同样不启动 SPA:旧路径 404,不吐 index.html。
  const deadNav = await page.goto('/ui/')
  expect(deadNav?.status(), '/ui/ 浏览器导航').toBe(404)
  await expect(page.getByTestId(TID.loginUsername)).toHaveCount(0)
  // 资源后缀与禁目录列表不回退(noDirListing 契约)。
  expect((await request.get('/assets/nope.js')).status()).toBe(404)
  expect((await request.get('/assets/')).status()).toBe(404)
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
  // 刻意不注册 dialog 监听:新 SPA 的决定确认是应用内行内层,任何原生
  // dialog 都是契约违反——Playwright 默认自动 dismiss,旅程会在下一步响亮红。

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

  // ③ alice 审批台驳回门一 → 执行 FAILED(approval_rejected,fail-fast)。
  await approvalLogin(alice, ALICE)
  await approvalDecide(alice, 'gate1', execId, 'reject')
  await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  await expect(author.getByTestId(tid.nodeRow('gate1', 0))).toContainText('approval_rejected')

  // ④ 控制台回滚:选 gate1 行 → 回滚(单击布防 → 确认层确认,F10)→ 重挂起(round 2)。
  await author.getByTestId(tid.nodeRow('gate1', 0)).click()
  await author.getByTestId(TID.rollbackSubmit).click()
  await author.getByTestId(TID.rollbackConfirm).click()
  await expect(author.getByTestId(TID.execFlash)).toBeVisible()
  await expect(author.getByTestId(tid.nodeRow('gate1', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })

  // ⑤ alice 二轮批准 → 门二挂起 → bob 驳回(第二次 FAILED)。
  await approvalDecide(alice, 'gate1', execId, 'approve')
  await expect(author.getByTestId(tid.nodeRow('gate2', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await approvalLogin(bob, BOB)
  await approvalDecide(bob, 'gate2', execId, 'reject')

  // ⑥ 控制台续跑(无选点,自动定位首个 FAILED)→ 门二重挂起 → bob 批准。
  await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
  await author.getByTestId(TID.resumeSubmit).click()
  await expect(author.getByTestId(TID.execFlash)).toBeVisible()
  await expect(author.getByTestId(tid.nodeRow('gate2', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
  await approvalDecide(bob, 'gate2', execId, 'approve')

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
  const signPanel = aliceCanvas.getByTestId(tid.approvalEntry('sign'))
  await expect(signPanel).toBeVisible({ timeout: 15_000 })
  await expect(signPanel).toContainText('会签')
  // T23.10保留已结束gate1的决定历史；待投名单必须属于当前sign面板。
  await expect(signPanel.getByTestId(TID.pendingApprovers)).toHaveText('alice, bob')
  await approvalDecide(alice, 'sign', execId, 'approve')
  await expect(signPanel.getByTestId(TID.pendingApprovers)).toHaveText('bob', { timeout: 20_000 })

  // ⑨ bob 终票 → 终态 + 输出 + 审计链(reaper flush 后 node 级事件到场)。
  await approvalDecide(bob, 'sign', execId, 'approve')
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
