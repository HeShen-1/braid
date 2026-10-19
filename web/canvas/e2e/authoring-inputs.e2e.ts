// T23.7:从空白画布经 UI 编排、保存、发布并触发;API 只读对拍 DSL/执行结果。
// 无成品 definition 种子,无 LLM 调用。file_read 仅访问官方 e2e 白名单夹具。
import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { readFileSync } from 'node:fs'
import type { WorkflowDoc } from '../src/model/dsl.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

async function choose(page: Page, label: string, option: string) {
  await page.getByLabel(label, { exact: true }).click()
  await page.getByRole('option', { name: option, exact: true }).click()
}

async function moveNode(page: Page, id: string, x: number, y: number) {
  const box = await page.getByTestId(tid.nodeCard(id)).boundingBox()
  expect(box).not.toBeNull()
  await page.mouse.move(box!.x + 40, box!.y + 18)
  await page.mouse.down()
  await page.mouse.move(x + 40, y + 18, { steps: 12 })
  await page.mouse.up()
}

async function connect(page: Page, handle: string, target: string) {
  const source = await page.getByTestId(tid.nodeCard('branch')).locator(`[data-anchor="${handle}"]`).boundingBox()
  const dest = await page.getByTestId(tid.nodeCard(target)).locator('.react-flow__handle.target[data-side="top"]').boundingBox()
  expect(source).not.toBeNull()
  expect(dest).not.toBeNull()
  await page.mouse.move(source!.x + source!.width / 2, source!.y + source!.height / 2)
  await page.mouse.down()
  await page.mouse.move(dest!.x + dest!.width / 2, dest!.y + dest!.height / 2, { steps: 16 })
  await page.mouse.up()
}

async function canvasDoc(page: Page): Promise<WorkflowDoc> {
  return JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}') as WorkflowDoc
}

test('空白编排分支→存发与服务端DSL一致→缺必需字段就近提示→命中/默认两路真实执行', async ({ page, request }, testInfo) => {
  await page.setViewportSize({ width: 1680, height: 1050 })
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
  await page.getByTestId(TID.draftNew).click()
  await expect(page.getByText('空白画布', { exact: true })).toBeVisible()
  // 用移动手势安排本分支旅程的固定测试布局;连续新增的自然落位由独立旅程覆盖。
  await page.getByTestId(TID.panelLeftToggle).click()
  const canvas = await page.locator('.editor-canvas').boundingBox()
  expect(canvas).not.toBeNull()
  const name = `t237-authoring-${Date.now()}`
  await page.getByRole('button', { name: '工作流设置', exact: true }).click()
  await page.getByLabel('名称', { exact: true }).fill(name)
  const params = page.locator('.panel-section').filter({ has: page.getByRole('heading', { name: '触发参数', exact: true }) })
  for (const [i, field, description] of [[1, 'route', 'approved 命中条件，其余走默认'], [2, 'doc_path', 'file_read 白名单内的文档绝对路径']] as const) {
    await params.getByRole('button', { name: '添加', exact: true }).click()
    await page.getByLabel(`参数名 ${i}`, { exact: true }).fill(field)
    await page.getByRole('checkbox', { name: `参数必填 ${i}`, exact: true }).click()
    await page.getByLabel(`参数说明 ${i}`, { exact: true }).fill(description)
  }
  await page.getByTestId(tid.paletteBtn('branch')).click()
  await moveNode(page, 'branch', canvas!.x + 300, canvas!.y + 65)
  for (const [id, title, x] of [['tool', '条件命中读取', 75], ['tool_n2', '默认读取', 580]] as const) {
    await page.getByTestId(tid.paletteBtn('tool')).click()
    await moveNode(page, id, canvas!.x + x, canvas!.y + 360)
    await page.getByTestId(tid.nodeCard(id)).click()
    await page.getByLabel('标题', { exact: true }).fill(title)
    await page.getByLabel('工具名', { exact: true }).fill('file_read')
    const args = page.locator('.panel-section').filter({ has: page.getByRole('heading', { name: '工具入参', exact: true }) })
    await args.getByRole('button', { name: '添加', exact: true }).click()
    await page.getByLabel('入参名 1', { exact: true }).fill('path')
    await page.getByLabel('值 1', { exact: true }).fill('{{inputs.doc_path}}')
    const outputs = page.locator('.panel-section').filter({ has: page.getByRole('heading', { name: '输出声明', exact: true }) })
    await outputs.getByRole('button', { name: '添加', exact: true }).click()
    await page.getByLabel('输出名 1', { exact: true }).fill('content')
    await page.getByLabel('类型 1', { exact: true }).fill('string')
  }

  await page.getByTestId(tid.nodeCard('branch')).click()
  await page.getByRole('button', { name: '工作流设置', exact: true }).click()
  await expect(page.getByLabel('名称(已保存,改名 = 新建工作流)', { exact: true })).toHaveCount(0)
  await expect(page.getByLabel('名称', { exact: true })).toHaveValue(name)
  await expect(page.locator('.react-flow__node[data-id="branch"]')).not.toHaveClass(/selected/)
  await page.getByTestId(tid.nodeCard('branch')).click()
  await page.getByLabel('条件左值 1', { exact: true }).fill('{{inputs.route}}')
  await page.getByLabel('条件右值 1', { exact: true }).fill('approved')
  await choose(page, '条件去向 1', '默认读取 (tool_n2)')
  await choose(page, '默认去向', '默认读取 (tool_n2)')
  expect((await canvasDoc(page)).edges).toEqual([{ from: 'branch', to: 'tool_n2' }])
  // 实际拉线重连 case 出口,默认一路仍保留 tool_n2;然后同目标再连不造重复边。
  await connect(page, 'case_0', 'tool')
  await connect(page, 'default', 'tool_n2')
  await expect(page.getByLabel('条件去向 1', { exact: true })).toContainText('条件命中读取 (tool)')
  await expect(page.getByLabel('默认去向', { exact: true })).toContainText('默认读取 (tool_n2)')
  const routed = await canvasDoc(page)
  expect(routed.nodes.find((n) => n.id === 'branch')).toMatchObject({ cases: [{ when: { left: '{{inputs.route}}', op: 'eq', right: 'approved' }, next: 'tool' }], default: 'tool_n2' })
  expect(routed.edges).toHaveLength(2)

  for (const theme of ['dark', 'light'] as const) {
    await page.getByTestId(tid.themeOption(theme)).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    const shot = testInfo.outputPath(`authoring-${theme}.png`)
    await page.screenshot({ path: shot, fullPage: true, animations: 'disabled' })
    await testInfo.attach(`authoring-${theme}`, { path: shot, contentType: 'image/png' })
  }
  await page.setViewportSize({ width: 900, height: 700 })
  await page.getByText('DSL 预览', { exact: true }).click()
  await expect(page.locator('.react-flow__minimap')).toBeHidden()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  const narrow = testInfo.outputPath('authoring-narrow.png')
  await page.screenshot({ path: narrow, fullPage: true, animations: 'disabled' })
  await testInfo.attach('authoring-narrow', { path: narrow, contentType: 'image/png' })
  await page.getByText('DSL 预览', { exact: true }).click()
  await page.setViewportSize({ width: 1680, height: 1050 })
  await page.getByRole('button', { name: '隐藏缩略图', exact: true }).click()
  await expect(page.locator('.react-flow__minimap')).toHaveCount(0)
  await page.getByRole('button', { name: '显示缩略图', exact: true }).click()

  const current = await canvasDoc(page)
  const create = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/definitions')
  await page.getByTestId(TID.editorSave).click()
  const created = await create
  expect(created.status(), await created.text()).toBe(201)
  const { id } = await created.json() as { id: string }
  await expect(page.getByTestId(TID.editorPublish)).toBeEnabled()
  const publish = page.waitForResponse((r) => r.url().endsWith(`/drafts/${id}/publish`))
  await page.getByTestId(TID.editorPublish).click()
  const published = await publish
  expect(published.ok(), await published.text()).toBe(true)
  const { version } = await published.json() as { version: number }
  await expect(page.getByTestId(TID.workflowFlash)).toContainText(`已发布 ${name} v${version}`)

  const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const storedResponse = await request.get(`/api/v1/definitions/${name}/versions/${version}`, { headers })
  expect(storedResponse.ok()).toBe(true)
  const stored = await storedResponse.json() as { dsl: WorkflowDoc }
  expect(stored.dsl).toEqual(current)
  await testInfo.attach('canvas-and-server-dsl', { body: JSON.stringify({ canvas: current, server: stored.dsl }, null, 2), contentType: 'application/json' })

  await page.getByTestId(TID.viewPublished).click()
  await expect(page.getByRole('heading', { name: '已发布与版本历史' })).toBeVisible()
  await page.getByTestId(TID.navExecutions).click()
  await page.getByTestId(TID.triggerDefinition).click()
  await expect(page.getByRole('option', { name: '选择已发布工作流…', exact: true })).toBeDisabled()
  await page.getByRole('option', { name: `${name} v${version}`, exact: true }).click()
  const contract = page.getByLabel('所选版本输入要求', { exact: true })
  await expect(contract).toContainText(`输入要求 · ${name} v${version}`)
  await expect(contract).toContainText('file_read 白名单内的文档绝对路径')
  await expect(contract).toContainText('string · 必填')
  const posted: unknown[] = []
  page.on('request', (r) => { if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/executions') posted.push(r.postDataJSON()) })
  await page.getByTestId(TID.triggerSubmit).click()
  await expect(page.getByRole('alert')).toContainText('缺少必填参数：route、doc_path')
  expect(posted).toEqual([])
  for (const theme of ['dark', 'light'] as const) {
    await page.getByTestId(tid.themeOption(theme)).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    const shot = testInfo.outputPath(`inputs-${theme}.png`)
    await page.screenshot({ path: shot, fullPage: true, animations: 'disabled' })
    await testInfo.attach(`inputs-${theme}`, { path: shot, contentType: 'image/png' })
  }

  const docPath = path.resolve(process.cwd(), 'e2e/fixtures/files/a.txt')
  const content = readFileSync(docPath, 'utf8')
  for (const [route, active, skipped] of [['approved', 'tool', 'tool_n2'], ['other', 'tool_n2', 'tool']] as const) {
    await page.getByTestId(TID.triggerInput).fill(JSON.stringify({ route, doc_path: docPath }))
    const triggered = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/executions')
    await page.getByTestId(TID.triggerSubmit).click()
    const response = await triggered
    expect(response.status(), await response.text()).toBe(201)
    const ack = await response.json() as { id: string; definition_version: number }
    expect(ack.definition_version).toBe(version)
    await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 30_000 })
    await expect(page.getByTestId(tid.nodeRow(active, 0))).toHaveAttribute('data-state', 'SUCCEEDED')
    await expect(page.getByTestId(tid.nodeRow(skipped, 0))).toHaveAttribute('data-state', 'SKIPPED')
    const detailResponse = await request.get(`/api/v1/executions/${ack.id}`, { headers })
    expect(detailResponse.ok()).toBe(true)
    const detail = await detailResponse.json() as { definition_version: number; input: unknown; nodes: { node_id: string; status: string }[] }
    expect(detail.definition_version).toBe(version)
    expect(detail.input).toEqual({ route, doc_path: docPath })
    expect(detail.nodes.find((n) => n.node_id === active)?.status).toBe('SUCCEEDED')
    expect(detail.nodes.find((n) => n.node_id === skipped)?.status).toBe('SKIPPED')
    // 输出端点只作行为对拍,不是成品定义写入或预置执行。
    const output = await request.get(`/api/v1/executions/${ack.id}/nodes/${active}`, { headers })
    expect(output.ok()).toBe(true)
    const projection = await output.json() as { output: { content: string } }
    expect(projection.output.content).toBe(content)
    await testInfo.attach(`executed-${route}`, { body: JSON.stringify(detail, null, 2), contentType: 'application/json' })
  }
  expect(posted).toEqual(['approved', 'other'].map((route) => ({ definition_name: name, version, input: { route, doc_path: docPath } })))
})
