// T24.1：真实定义/执行/审批，验证折叠后的空间回收及同页标签；不调用模型。
import { expect, test, type Page } from '@playwright/test'
import { mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { TID, tid } from '../src/ui/testids.ts'

async function login(page: Page, user: string) {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(user)
  await page.getByTestId(TID.loginPassword).fill(`e2e-${user}-pass-1`)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.shell)).toBeVisible()
}

let graphCapture = 0
async function graphFits(page: Page) {
  const graph = page.locator('.exec-graph-canvas')
  await expect.poll(async () => graph.evaluate((el) => {
    const bounds = el.getBoundingClientRect()
    const nodes = [...el.querySelectorAll('.react-flow__node')]
    return nodes.length === 4 && nodes.every((node) => {
      const b = node.getBoundingClientRect()
      return b.width > 0 && b.left >= bounds.left - 1 && b.right <= bounds.right + 1 && b.top >= bounds.top - 1 && b.bottom <= bounds.bottom + 1
    })
  })).toBe(true)
  // Compare consecutive painted frames, not a fixed timeout or merely document overflow.
  await expect.poll(() => graph.evaluate(async (el) => {
    const viewport = el.querySelector('.react-flow__viewport') as HTMLElement
    const before = viewport.style.transform
    await new Promise(requestAnimationFrame)
    await new Promise(requestAnimationFrame)
    return before === viewport.style.transform
  })).toBe(true)
  const evidence = test.info().outputPath(`definition-graph-${++graphCapture}.json`)
  await writeFile(evidence, JSON.stringify(await graph.evaluate((el) => ({
    viewport: { width: innerWidth, height: innerHeight },
    container: el.getBoundingClientRect().toJSON(),
    transform: (el.querySelector('.react-flow__viewport') as HTMLElement).style.transform,
    nodes: [...el.querySelectorAll('.react-flow__node')].map((node) => ({ id: node.getAttribute('data-id'), bounds: node.getBoundingClientRect().toJSON() })),
  })), null, 2))
  await test.info().attach('definition-graph-bounds', { contentType: 'application/json', path: evidence })
}

test('B v2：宽画布、可恢复侧栏、执行证据标签与真实审批记录', async ({ page, request, browser }) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const auth = await request.post('/api/v1/auth/login', { data: { username: 'author', password: 'e2e-author-pass-1' } })
  expect(auth.ok()).toBe(true)
  const { token } = await auth.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const name = `t241-document-review-${Date.now()}`
  const ids = ['source', 'attachment', 'review', 'archive']
  const titles = ['读取正文', '读取附件', '人工审批', '读取归档材料']
  const file = path.resolve('e2e/fixtures/files/a.txt')
  const dsl = { dsl: '1.0', name,
    nodes: ids.map((id, i) => id === 'review'
      ? { id, title: titles[i], type: 'approval', mode: 'any', approvers: ['alice'], instructions: '布局联调测试材料。请核对正文及附件，确认后继续归档。此执行仅读取本地测试文件，不调用模型。', outputs: { decision: 'string' } }
      : { id, title: titles[i], type: 'tool', tool: 'file_read', inputs: { path: file }, outputs: { content: 'string' } }),
    edges: ids.slice(1).map((id, i) => ({ from: ids[i], to: id })),
    metadata: { canvas: { nodes: Object.fromEntries(ids.map((id, i) => [id, { position: { x: 60 + i * 260, y: 160 } }])), edges: ids.slice(1).map((id, i) => ({ from: ids[i], to: id, source_side: 'right', target_side: 'left' })) } },
  }
  const draft = await request.post('/api/v1/definitions', { headers, data: dsl })
  expect(draft.ok(), await draft.text()).toBe(true)
  const { id } = await draft.json() as { id: string }
  const shots = path.resolve('../../.impeccable/review/t24.1')
  await mkdir(shots, { recursive: true })
  const capture = async (target: Page, label: string) => {
    await target.evaluate(async () => { await document.fonts.ready })
    await expect.poll(() => target.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    if (process.env.BRAID_LAYOUT_CAPTURE === '1') await target.screenshot({ path: path.join(shots, `${label}.png`), fullPage: true, animations: 'disabled' })
  }
  await page.setViewportSize({ width: 1600, height: 1000 })
  await login(page, 'author')
  await page.goto(`/workflows/${id}`)
  await expect(page.getByTestId(tid.nodeCard('review'))).toBeVisible()
  await expect(page.getByRole('button', { name: '展开工作流列表' })).toBeVisible()
  await expect(page.getByRole('button', { name: '展开属性面板' })).toBeVisible()
  await capture(page, 'workflow-desktop')
  const before = await page.locator('.editor-canvas').boundingBox()
  await page.getByRole('button', { name: '收起侧栏' }).click()
  await expect(page.getByRole('navigation', { name: '主域导航' })).toBeHidden()
  await expect(page.getByRole('button', { name: '展开侧栏' })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: '收起侧栏' })).toBeFocused()
  await page.keyboard.press('Enter')
  const after = await page.locator('.editor-canvas').boundingBox()
  expect(after!.width - before!.width).toBeGreaterThan(180)
  await page.reload()
  await expect(page.getByRole('button', { name: '展开侧栏' })).toBeVisible()
  for (const width of [1024, 390]) {
    await page.setViewportSize({ width, height: 900 })
    await capture(page, `workflow-${width}`)
  }
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.getByRole('button', { name: '展开侧栏' }).click()
  // 保存/发布走真实 UI；发布的 DSL 必须匹配编辑器。
  const expected = JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}')
  const published = page.waitForResponse((res) => res.url().endsWith(`/drafts/${id}/publish`))
  await page.getByTestId(TID.editorPublish).click()
  const publish = await published
  expect(publish.ok()).toBe(true)
  const { version } = await publish.json() as { version: number }
  const stored = await request.get(`/api/v1/definitions/${name}/versions/${version}`, { headers })
  const saved = (await stored.json()).dsl
  // Go/JS 浮点编码允许 viewport zoom 的末位舍入，业务 DSL 及其他布局字段须完全一致。
  expect(saved.metadata.canvas.viewport.zoom).toBeCloseTo(expected.metadata.canvas.viewport.zoom, 12)
  expect({ ...saved, metadata: { ...saved.metadata, canvas: { ...saved.metadata.canvas, viewport: { ...saved.metadata.canvas.viewport, zoom: expected.metadata.canvas.viewport.zoom } } } }).toEqual(expected)
  const triggered = await request.post('/api/v1/executions', { headers: { ...headers, 'Idempotency-Key': name }, data: { definition_name: name, version, input: {} } })
  expect(triggered.ok(), await triggered.text()).toBe(true)
  const { id: execId } = await triggered.json() as { id: string }
  await expect.poll(async () => (await (await request.get(`/api/v1/executions/${execId}`, { headers })).json()).status, { timeout: 30000 }).toBe('WAITING_APPROVAL')
  await page.goto(`/executions/${execId}`)
  await page.getByRole('button', { name: '收起侧栏' }).click()
  await expect(page.getByTestId(tid.nodeCard('review'))).toBeVisible()
  await expect(page.getByRole('tabpanel', { name: '时间线' })).toBeVisible()
  await graphFits(page)
  await capture(page, 'execution-desktop')
  await page.getByRole('button', { name: '收起执行列表' }).click()
  await expect(page.getByLabel('执行列表与触发')).toBeHidden()
  await page.getByRole('button', { name: '收起面板', exact: true }).click()
  await expect(page.getByRole('tabpanel', { name: '时间线' })).toBeHidden()
  await expect(page.getByRole('button', { name: '取消执行', exact: true })).toBeInViewport()
  await page.getByRole('tab', { name: '输入与输出' }).click()
  await expect(page.getByRole('tabpanel', { name: '输入与输出' })).toBeVisible()
  await page.getByRole('tab', { name: '审计记录' }).click()
  await expect(page.getByRole('heading', { name: '审计事件', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '取消执行', exact: true })).toBeInViewport()
  const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } })
  const approver = await context.newPage()
  approver.on('pageerror', (error) => errors.push(error.message))
  await login(approver, 'alice')
  await approver.goto(`/approvals/${execId}/review`)
  await expect(approver.getByTestId(TID.approvalInstructions)).toBeVisible()
  await approver.getByTestId(TID.approvalComment).fill('真实联调：材料核对完成')
  await capture(approver, 'approval-content-desktop')
  await approver.getByRole('tab', { name: '审批内容', exact: true }).click()
  await approver.keyboard.press('ArrowRight')
  await expect(approver.getByRole('tab', { name: '审批记录', exact: true })).toBeFocused()
  await expect(approver.getByTestId(TID.approvalInstructions)).toBeHidden()
  await expect(approver.getByText('尚无审批决定，等待审批人处理。')).toBeVisible()
  await approver.getByRole('button', { name: '收起侧栏' }).click()
  await capture(approver, 'approval-history-desktop')
  await approver.getByRole('button', { name: '收起处理栏' }).click()
  await expect(approver.getByTestId(TID.approvalComment)).toBeHidden()
  let decisionPosts = 0
  approver.on('request', (req) => { if (req.method() === 'POST' && new URL(req.url()).pathname === `/api/v1/executions/${execId}/approvals/review/decision`) decisionPosts++ })
  await approver.keyboard.press('r')
  await expect(approver.getByRole('group', { name: '确认驳回' })).toBeFocused()
  await approver.keyboard.press('Escape')
  await expect(approver.getByRole('button', { name: '展开处理栏' })).toBeFocused()
  expect(decisionPosts).toBe(0)
  await approver.keyboard.press('Enter')
  await expect(approver.getByTestId(TID.approvalComment)).toHaveValue('真实联调：材料核对完成')
  await approver.keyboard.press('r')
  await approver.keyboard.press('Escape')
  await expect(approver.getByTestId(TID.approvalReject)).toBeFocused()
  await approver.getByRole('button', { name: '收起处理栏' }).click()
  await approver.keyboard.press('r')
  await approver.getByRole('button', { name: '放弃', exact: true }).click()
  await expect(approver.getByRole('button', { name: '展开处理栏' })).toBeFocused()
  expect(decisionPosts).toBe(0)
  await approver.keyboard.press('Enter')
  await expect(approver.getByTestId(TID.approvalComment)).toHaveValue('真实联调：材料核对完成')
  for (const width of [1024, 390]) {
    await approver.setViewportSize({ width, height: 900 })
    await approver.getByRole('tab', { name: '审批内容', exact: true }).click()
    await capture(approver, `approval-${width}`)
    await page.setViewportSize({ width, height: 900 })
    await page.getByRole('tab', { name: '时间线', exact: true }).click()
    await graphFits(page)
    await expect(page.getByRole('button', { name: '取消执行', exact: true })).toBeInViewport()
    await capture(page, `execution-${width}`)
  }
  await page.setViewportSize({ width: 1600, height: 1000 })
  await graphFits(page)
  await page.getByRole('button', { name: '展开执行列表' }).click()
  await graphFits(page)
  await page.getByRole('button', { name: '展开侧栏' }).click()
  await graphFits(page)
  await page.getByRole('button', { name: '收起定义版本图' }).click()
  await page.getByRole('button', { name: '展开定义版本图' }).click()
  await graphFits(page)
  await approver.getByTestId(TID.approvalApprove).click()
  await expect(approver.getByTestId(TID.approvalFlash)).toContainText('已记录:批准')
  expect(decisionPosts).toBe(1)
  await approver.goto(`/approvals/${execId}/review`)
  await approver.getByRole('tab', { name: '审批记录', exact: true }).click()
  await expect(approver.getByTestId(TID.approvalTimeline)).toContainText('真实联调：材料核对完成')
  await expect.poll(async () => (await (await request.get(`/api/v1/executions/${execId}`, { headers })).json()).status, { timeout: 30000 }).toBe('SUCCEEDED')
  expect(errors).toEqual([])
  await context.close()
})
