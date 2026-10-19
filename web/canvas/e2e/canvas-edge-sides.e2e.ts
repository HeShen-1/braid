// UX-F2/#228:普通四向端点与分支兼容。新图只经 UI 编排/拖线/保存;
// 旧文档由 API 仅作兼容 setup,与真实编辑操作和保存请求分别留证。
import { expect, test, type APIRequestContext, type Page, type TestInfo } from '@playwright/test'
import path from 'node:path'
import { writeFile } from 'node:fs/promises'
import type { WorkflowDoc } from '../src/model/dsl.ts'
import { loadBase } from '../src/model/testdata.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }
type Side = 'top' | 'right' | 'bottom' | 'left'
type Endpoint = { node: string; side: Side; anchor?: string }
type Connection = { source: Endpoint; target: Endpoint }
type Headers = Record<string, string>

async function attachJson(testInfo: TestInfo, name: string, value: unknown) {
  const file = testInfo.outputPath(`${name}.json`)
  await writeFile(file, JSON.stringify(value, null, 2))
  await testInfo.attach(name, { path: file, contentType: 'application/json' })
}

async function canvasDoc(page: Page): Promise<WorkflowDoc> {
  return JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}') as WorkflowDoc
}

async function login(page: Page) {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navWorkflows)).toBeVisible()
}

async function authHeaders(request: APIRequestContext): Promise<Headers> {
  const response = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(response.ok()).toBe(true)
  const { token } = await response.json() as { token: string }
  return { Authorization: `Bearer ${token}` }
}

async function collapsePanels(page: Page) {
  const left = page.getByTestId(TID.panelLeftToggle)
  if (await left.getAttribute('aria-label') === '收起工作流列表') await left.click()
  const right = page.getByTestId(TID.panelRightToggle)
  if (await right.getAttribute('aria-pressed') === 'true') await right.click()
}

function handleSelector(endpoint: Endpoint, kind: 'source' | 'target'): string {
  const anchor = endpoint.anchor === undefined ? '' : `[data-anchor="${endpoint.anchor}"]`
  return `[data-testid="${tid.nodeCard(endpoint.node)}"] .react-flow__handle.${kind}[data-side="${endpoint.side}"]${anchor}`
}

function edgeSelector(connection: Connection): string {
  return `.react-flow__edge[data-id="e:${connection.source.node}:${connection.target.node}"] .react-flow__edge-path`
}

async function handleVisibility(page: Page, endpoint: Endpoint, kind: 'source' | 'target') {
  return page.locator(handleSelector(endpoint, kind)).evaluate((element) => {
    const rect = element.getBoundingClientRect()
    const canvas = element.closest('.editor-canvas')!.getBoundingClientRect()
    const windowBounds = { left: 0, top: 0, right: innerWidth, bottom: innerHeight }
    // IntersectionObserver 的 ratio=1 会受缩放浮点误差影响;仍要求完整入界。
    const epsilon = 0.001
    const inside = (bounds: { left: number; top: number; right: number; bottom: number }) =>
      rect.left >= bounds.left - epsilon && rect.top >= bounds.top - epsilon &&
      rect.right <= bounds.right + epsilon && rect.bottom <= bounds.bottom + epsilon
    let cssVisible = true
    let opacity = 1
    for (let el: Element | null = element; el !== null; el = el.parentElement) {
      const style = getComputedStyle(el)
      cssVisible &&= style.display !== 'none' && style.visibility === 'visible'
      opacity *= Number.parseFloat(style.opacity)
    }
    const insideWindow = inside(windowBounds)
    const insideCanvas = inside(canvas)
    return {
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
      canvas: { left: canvas.left, top: canvas.top, right: canvas.right, bottom: canvas.bottom },
      windowBounds, epsilon, cssVisible, opacity, insideWindow, insideCanvas,
      visible: rect.width > 0 && rect.height > 0 && cssVisible && opacity > 0 && insideWindow && insideCanvas,
    }
  })
}

async function assertHandleVisible(page: Page, endpoint: Endpoint, kind: 'source' | 'target') {
  await expect.poll(async () => (await handleVisibility(page, endpoint, kind)).visible, {
    message: `${endpoint.node} ${kind} ${endpoint.side}: 手柄须完整位于窗口及画布内`,
  }).toBe(true)
  return handleVisibility(page, endpoint, kind)
}

async function edgeGeometry(page: Page, connection: Connection) {
  return page.locator(edgeSelector(connection)).evaluate((element, selectors) => {
    const edge = element as SVGPathElement
    const matrix = edge.getScreenCTM()
    if (matrix === null) throw new Error('连线没有屏幕变换矩阵')
    const point = (distance: number) => {
      const local = edge.getPointAtLength(distance)
      const screen = new DOMPoint(local.x, local.y).matrixTransform(matrix)
      return { x: screen.x, y: screen.y }
    }
    const handle = (selector: string) => {
      const el = document.querySelector<HTMLElement>(selector)
      if (el === null) throw new Error(`所选端点未呈现: ${selector}`)
      const { x, y, width, height } = el.getBoundingClientRect()
      const side = el.dataset.side
      // SVG 接在手柄朝向的外沿;不是只核对 DSL 或复制内部 handle 绑定。
      return {
        id: el.dataset.handleid ?? null, side, rect: { x, y, width, height },
        point: {
          x: side === 'left' ? x : side === 'right' ? x + width : x + width / 2,
          y: side === 'top' ? y : side === 'bottom' ? y + height : y + height / 2,
        },
      }
    }
    const source = handle(selectors.source)
    const target = handle(selectors.target)
    const length = edge.getTotalLength()
    const start = point(0)
    const end = point(length)
    const style = getComputedStyle(edge)
    const colorAlpha = style.stroke === 'transparent' ? 0 : style.stroke.startsWith('rgba(')
      ? Number.parseFloat(style.stroke.slice(style.stroke.lastIndexOf(',') + 1))
      : style.stroke.includes('/') ? Number.parseFloat(style.stroke.slice(style.stroke.lastIndexOf('/') + 1)) : 1
    let cssVisible = true
    let opacity = Number.parseFloat(style.strokeOpacity) * colorAlpha
    for (let el: Element | null = edge; el !== null; el = el.parentElement) {
      const css = getComputedStyle(el)
      cssVisible &&= css.display !== 'none' && css.visibility === 'visible'
      opacity *= Number.parseFloat(css.opacity)
    }
    const canvas = edge.closest('.editor-canvas')!
    const bounds = canvas.getBoundingClientRect()
    const obstacles = Array.from(canvas.querySelectorAll('.react-flow__node, .react-flow__minimap, .react-flow__controls'))
      .map((element) => element.getBoundingClientRect())
    const scale = Math.max(Math.hypot(matrix.a, matrix.b), Math.hypot(matrix.c, matrix.d))
    const sampleCount = Math.min(4096, Math.max(64, Math.ceil(length * scale / 2)))
    const samples = Array.from({ length: sampleCount + 1 }, (_, i) => {
      const p = point(length * i / sampleCount)
      const inCanvas = p.x >= Math.max(0, bounds.left) && p.y >= Math.max(0, bounds.top) &&
        p.x <= Math.min(innerWidth, bounds.right) && p.y <= Math.min(innerHeight, bounds.bottom)
      const obscured = obstacles.some((r) => p.x >= r.left && p.x <= r.right && p.y >= r.top && p.y <= r.bottom)
      return { ...p, exposed: inCanvas && !obscured }
    })
    const exposedSegments = samples.slice(1).filter((p, i) => p.exposed && samples[i].exposed &&
      Math.hypot(p.x - samples[i].x, p.y - samples[i].y) > 0.001).length
    const strokeWidth = Number.parseFloat(style.strokeWidth)
    const rect = edge.getBoundingClientRect()
    // 有 stroke 的直线可合法零宽/零高。核实际可见线段,不要求填充包围盒有面积。
    const paint = {
      length, stroke: style.stroke, strokeWidth, screenStrokeWidth: strokeWidth * scale,
      cssVisible, opacity, sampleCount, exposedSegments,
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
      exposedSamples: samples.filter((p) => p.exposed).slice(0, 16),
      visible: Number.isFinite(length) && length > 0 && style.stroke !== 'none' && strokeWidth * scale > 0 &&
        cssVisible && opacity > 0 && exposedSegments > 0 && samples.every((p) => Number.isFinite(p.x) && Number.isFinite(p.y)),
    }
    return {
      d: edge.getAttribute('d'), start, end, source, target, paint,
      sourceError: Math.hypot(start.x - source.point.x, start.y - source.point.y),
      targetError: Math.hypot(end.x - target.point.x, end.y - target.point.y),
    }
  }, { source: handleSelector(connection.source, 'source'), target: handleSelector(connection.target, 'target') })
}

async function verifyEdges(page: Page, connections: Connection[], testInfo: TestInfo, phase: string) {
  const measured = []
  for (const connection of connections) {
    const sourceVisibility = await assertHandleVisible(page, connection.source, 'source')
    const targetVisibility = await assertHandleVisible(page, connection.target, 'target')
    try {
      await expect.poll(async () => {
        const current = await edgeGeometry(page, connection)
        return current.d !== null && current.paint.visible && current.sourceError <= 2 && current.targetError <= 2
      }, { message: `${phase}: SVG 两端必须落在所选手柄`, timeout: 10_000 }).toBe(true)
    } finally {
      await attachJson(testInfo, `${phase}-${connection.source.node}-${connection.target.node}`, { connection, sourceVisibility, targetVisibility, geometry: await edgeGeometry(page, connection) })
    }
    measured.push(await edgeGeometry(page, connection))
  }
  const doc = await canvasDoc(page)
  const layout = await page.locator('.editor-canvas').evaluate((canvas, positions) => {
    const flow = canvas.querySelector('.react-flow')!
    const origin = flow.getBoundingClientRect()
    const viewport = canvas.querySelector('.react-flow__viewport')!
    const transform = new DOMMatrix(getComputedStyle(viewport).transform)
    return Array.from(canvas.querySelectorAll<HTMLElement>('.react-flow__node')).map((node) => {
      const id = node.dataset.id ?? ''
      const rect = node.getBoundingClientRect()
      const actual = { x: (rect.x - origin.x - transform.e) / transform.a, y: (rect.y - origin.y - transform.f) / transform.d }
      const expected = positions?.[id]?.position
      return { id, actual, expected, matches: expected !== undefined && Math.abs(actual.x - expected.x) <= 1 && Math.abs(actual.y - expected.y) <= 1 }
    })
  }, doc.metadata?.canvas?.nodes)
  expect(layout).toHaveLength(doc.nodes.length)
  expect(layout.every((node) => node.matches), `${phase}: 实际节点位置与 DSL 一致`).toBe(true)
  await attachJson(testInfo, `${phase}-dsl-and-layout`, { dsl: doc, layout, edges: measured })
  const screenshot = testInfo.outputPath(`${phase}.png`)
  await page.screenshot({ path: screenshot, fullPage: true, animations: 'allow' })
  await testInfo.attach(phase, { path: screenshot, contentType: 'image/png' })
}

async function dragConnect(page: Page, connection: Connection) {
  const source = page.locator(handleSelector(connection.source, 'source'))
  const target = page.locator(handleSelector(connection.target, 'target'))
  await assertHandleVisible(page, connection.source, 'source')
  await assertHandleVisible(page, connection.target, 'target')
  const from = await source.boundingBox()
  const to = await target.boundingBox()
  expect(from).not.toBeNull()
  expect(to).not.toBeNull()
  await page.mouse.move(from!.x + from!.width / 2, from!.y + from!.height / 2)
  await page.mouse.down()
  await page.mouse.move(to!.x + to!.width / 2, to!.y + to!.height / 2, { steps: 24 })
  await page.mouse.up()
  await expect.poll(async () => {
    const current = await edgeGeometry(page, connection)
    return current.paint.visible && current.sourceError <= 2 && current.targetError <= 2
  }, { message: '真实拖线后 SVG 必须改绑到选定端点' }).toBe(true)
}

async function saveAndRead(page: Page, request: APIRequestContext, headers: Headers, testInfo: TestInfo, phase: string, id?: string) {
  const expected = await canvasDoc(page)
  const pathname = id === undefined ? '/api/v1/definitions' : `/api/v1/drafts/${id}`
  const method = id === undefined ? 'POST' : 'PUT'
  const pending = page.waitForResponse((response) => new URL(response.url()).pathname === pathname && response.request().method() === method)
  await page.getByTestId(TID.editorSave).click()
  const response = await pending
  expect(response.ok(), await response.text()).toBe(true)
  const sent = response.request().postDataJSON() as WorkflowDoc
  expect(sent).toEqual(expected)
  const saved = await response.json() as { id: string }
  if (id !== undefined) expect(saved.id).toBe(id)
  const read = await request.get(`/api/v1/drafts/${saved.id}`, { headers })
  expect(read.ok()).toBe(true)
  const detail = await read.json() as { status: string; dsl: WorkflowDoc }
  expect(detail.status).toBe('DRAFT')
  expect(detail.dsl).toEqual(expected)
  await expect(page.getByTestId(TID.editorFlash)).toContainText(/已保存|已创建/)
  await attachJson(testInfo, `${phase}-request-server`, { id: saved.id, request: sent, server: detail.dsl })
  return { id: saved.id, doc: expected }
}

async function reopen(page: Page, id: string, expected: WorkflowDoc): Promise<Page> {
  const context = page.context()
  const viewport = page.viewportSize()!
  await page.close()
  const opened = await context.newPage()
  await opened.setViewportSize(viewport)
  // 新 Page 的 sessionStorage 不继承旧 Page,真实重新登录后从服务端打开。
  await login(opened)
  const loaded = opened.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/drafts/${id}` && response.request().method() === 'GET')
  await opened.goto(`/workflows/${id}`)
  expect((await loaded).ok()).toBe(true)
  await expect(opened.getByTestId(TID.editor)).toBeVisible()
  await expect.poll(() => canvasDoc(opened)).toEqual(expected)
  return opened
}

async function createTool(page: Page, title: string): Promise<string> {
  const before = await canvasDoc(page)
  await page.getByTestId(tid.paletteBtn('tool')).click()
  await expect(page.locator('.react-flow__node')).toHaveCount(before.nodes.length + 1)
  const added = (await canvasDoc(page)).nodes.filter((node) => !before.nodes.some((old) => old.id === node.id))
  expect(added).toHaveLength(1)
  const id = added[0].id
  await expect(page.getByTestId(TID.nodePanel).locator('header code')).toHaveText(id)
  await page.getByLabel('标题', { exact: true }).fill(title)
  await page.getByLabel('工具名', { exact: true }).fill('file_read')
  const inputs = page.getByTestId(TID.nodePanel).locator('.panel-section').filter({ has: page.getByRole('heading', { name: '工具入参', exact: true }) })
  await inputs.getByRole('button', { name: '添加', exact: true }).click()
  await page.getByLabel('入参名 1', { exact: true }).fill('path')
  await page.getByLabel('值 1', { exact: true }).fill(path.resolve(process.cwd(), 'e2e/fixtures/files/a.txt'))
  return id
}

test('四向普通端点:真实拖线、保存读回、关闭重开、改绑、删除与发布 (1440 dark)', async ({ page, request }, testInfo) => {
  let editor = page
  await editor.setViewportSize({ width: 1440, height: 900 })
  const headers = await authHeaders(request)
  await login(editor)
  await editor.getByTestId(tid.themeOption('dark')).click()
  await editor.getByTestId(TID.draftNew).click()
  await editor.getByTestId(TID.panelLeftToggle).click()
  await editor.getByRole('button', { name: '工作流设置', exact: true }).click()
  await editor.getByLabel('名称', { exact: true }).fill(`ux-f2-sides-${Date.now()}`)
  const a = await createTool(editor, '起点读取')
  const b = await createTool(editor, '中间读取')
  const c = await createTool(editor, '终点读取')
  await collapsePanels(editor)

  const first: Connection[] = [
    { source: { node: a, side: 'right' }, target: { node: b, side: 'left' } },
    { source: { node: b, side: 'top' }, target: { node: c, side: 'bottom' } },
  ]
  for (const connection of first) await dragConnect(editor, connection)
  expect((await canvasDoc(editor)).metadata?.canvas?.edges).toEqual([
    { from: a, to: b, source_side: 'right', target_side: 'left' },
    { from: b, to: c, source_side: 'top', target_side: 'bottom' },
  ])
  const firstSave = await saveAndRead(editor, request, headers, testInfo, 'ordinary-first')
  await verifyEdges(editor, first, testInfo, 'ordinary-before-close')
  editor = await reopen(editor, firstSave.id, firstSave.doc)
  await expect(editor.locator('html')).toHaveAttribute('data-theme', 'dark')
  await verifyEdges(editor, first, testInfo, 'ordinary-reopened')

  // 同一对节点改端点,不增加并行边;默认 bottom/top 不冗余落盘。
  const rebound: Connection[] = [
    { source: { node: a, side: 'left' }, target: { node: b, side: 'right' } },
    { source: { node: b, side: 'bottom' }, target: { node: c, side: 'top' } },
  ]
  for (const connection of rebound) await dragConnect(editor, connection)
  expect((await canvasDoc(editor)).edges).toHaveLength(2)
  expect((await canvasDoc(editor)).metadata?.canvas?.edges).toEqual([{ from: a, to: b, source_side: 'left', target_side: 'right' }])
  const secondSave = await saveAndRead(editor, request, headers, testInfo, 'ordinary-rebound', firstSave.id)
  editor = await reopen(editor, secondSave.id, secondSave.doc)
  await verifyEdges(editor, rebound, testInfo, 'ordinary-rebound-reopened')

  // 删除有方向记录的目标,保留另一条侧边再发布;取消仍保持原图与焦点。
  await dragConnect(editor, first[1])
  const beforeDelete = await canvasDoc(editor)
  expect(beforeDelete.metadata?.canvas?.edges).toContainEqual({ from: b, to: c, source_side: 'top', target_side: 'bottom' })
  await editor.getByTestId(TID.panelRightToggle).click()
  await editor.getByTestId(tid.nodeCard(c)).click()
  await editor.getByTestId(TID.nodeDelete).click()
  await expect(editor.getByTestId(TID.deleteConfirm)).toBeVisible()
  await editor.getByRole('button', { name: '放弃', exact: true }).click()
  await expect(editor.getByTestId(TID.nodeDelete)).toBeFocused()
  expect(await canvasDoc(editor)).toEqual(beforeDelete)
  await editor.getByTestId(TID.nodeDelete).press('Enter')
  await editor.getByTestId(TID.deleteConfirmSubmit).click()
  await expect(editor.getByTestId(tid.nodeCard(c))).toHaveCount(0)
  const deleted = await canvasDoc(editor)
  expect(deleted.nodes).toEqual(beforeDelete.nodes.filter((node) => node.id !== c))
  expect(deleted.edges).toEqual([{ from: a, to: b }])
  expect(deleted.metadata?.canvas?.nodes).not.toHaveProperty(c)
  expect(deleted.metadata?.canvas?.edges).toEqual([{ from: a, to: b, source_side: 'left', target_side: 'right' }])
  const savedBeforePublish = editor.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/drafts/${firstSave.id}` && response.request().method() === 'PUT')
  const published = editor.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/drafts/${firstSave.id}/publish`)
  await editor.getByTestId(TID.editorPublish).click()
  const saveResponse = await savedBeforePublish
  expect(saveResponse.ok()).toBe(true)
  expect(saveResponse.request().postDataJSON()).toEqual(deleted)
  const publishResponse = await published
  expect(publishResponse.ok(), await publishResponse.text()).toBe(true)
  const { version } = await publishResponse.json() as { version: number }
  const stored = await request.get(`/api/v1/definitions/${deleted.name}/versions/${version}`, { headers })
  expect(stored.ok()).toBe(true)
  const detail = await stored.json() as { dsl: WorkflowDoc }
  expect(detail.dsl).toEqual(deleted)
  await expect(editor.getByTestId(TID.workflowFlash)).toContainText(`已发布 ${deleted.name}`)
  await attachJson(testInfo, 'ordinary-published-server', detail.dsl)
})

test('分支旧文档兼容:无记录推导、显式未绑定、case/default 与四向目标 (1024 light)', async ({ page, request }, testInfo) => {
  let editor = page
  await editor.setViewportSize({ width: 1024, height: 900 })
  const headers = await authHeaders(request)
  await login(editor)
  await editor.getByTestId(tid.themeOption('light')).click()
  for (const mode of ['no-record', 'unbound'] as const) {
    const setup = loadBase('with-positions')
    setup.name = `ux-f2-${mode}-${Date.now()}`
    setup.metadata!.canvas!.viewport = { x: 0, y: 0, zoom: 0.35 }
    if (mode === 'no-record') delete setup.metadata!.canvas!.edges
    else setup.metadata!.canvas!.edges = [{ from: 'verdict', to: 'report' }, { from: 'verdict', to: 'revise', source_anchor: 'default' }]
    // 仅 setup 使用 API 写入旧格式文档;下文拖线/保存都走可见 UI。
    const created = await request.post('/api/v1/definitions', { headers, data: setup })
    expect(created.ok(), await created.text()).toBe(true)
    const { id } = await created.json() as { id: string }
    await attachJson(testInfo, `${mode}-compatibility-api-setup`, setup)
    await editor.goto(`/workflows/${id}`)
    await expect(editor.getByTestId(TID.editor)).toBeVisible()
    await collapsePanels(editor)
    const before = await canvasDoc(editor)
    const branch = before.nodes.find((node) => node.id === 'verdict')
    const initial: Connection[] = [
      { source: { node: 'verdict', side: 'bottom', anchor: mode === 'no-record' ? 'case_0' : 'fallback' }, target: { node: 'report', side: 'top' } },
      { source: { node: 'verdict', side: 'bottom', anchor: 'default' }, target: { node: 'revise', side: 'top' } },
    ]
    await verifyEdges(editor, initial, testInfo, `${mode}-legacy-open`)
    expect(before.metadata?.canvas?.edges).toContainEqual(mode === 'no-record'
      ? { from: 'verdict', to: 'report', source_anchor: 'case_0' }
      : { from: 'verdict', to: 'report' })
    const edited: Connection[] = [
      { source: { node: 'review', side: 'right' }, target: { node: 'verdict', side: 'left' } },
      { source: { node: 'verdict', side: 'bottom', anchor: 'case_0' }, target: { node: 'report', side: 'left' } },
      { source: { node: 'verdict', side: 'bottom', anchor: 'default' }, target: { node: 'revise', side: 'right' } },
    ]
    for (const connection of edited) await dragConnect(editor, connection)
    const after = await canvasDoc(editor)
    expect(after.nodes.find((node) => node.id === 'verdict')).toEqual(branch)
    expect(after.edges).toEqual(before.edges)
    expect(after.metadata?.canvas?.edges).toContainEqual({ from: 'review', to: 'verdict', source_side: 'right', target_side: 'left' })
    expect(after.metadata?.canvas?.edges).toContainEqual({ from: 'verdict', to: 'report', source_anchor: 'case_0', target_side: 'left' })
    expect(after.metadata?.canvas?.edges).toContainEqual({ from: 'verdict', to: 'revise', source_anchor: 'default', target_side: 'right' })
    await editor.getByTestId(TID.panelRightToggle).click()
    await editor.getByTestId(tid.nodeCard('verdict')).click()
    await expect(editor.getByLabel('条件去向 1', { exact: true })).toContainText('最终报告 (report)')
    await expect(editor.getByLabel('默认去向', { exact: true })).toContainText('修订建议 (revise)')
    await collapsePanels(editor)
    const saved = await saveAndRead(editor, request, headers, testInfo, `${mode}-edited`, id)
    editor = await reopen(editor, saved.id, saved.doc)
    await expect(editor.locator('html')).toHaveAttribute('data-theme', 'light')
    await verifyEdges(editor, edited, testInfo, `${mode}-edited-reopened`)
  }
})
