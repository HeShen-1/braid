// UX-F1-a：正式 dist、真实账号/API/worker，独立审批节点，零模型调用。
// 同一条真实待办覆盖双主题/1440与1024；截屏、正文/描边/焦点对比度分开留证。
import { expect, test, type APIRequestContext, type Page, type TestInfo } from '@playwright/test'
import { TID, tid } from '../src/ui/testids.ts'

async function seedApproval(request: APIRequestContext) {
  const login = await request.post('/api/v1/auth/login', { data: { username: 'author', password: 'e2e-author-pass-1' } })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const name = `ux-f1-a-${Date.now()}-document-review-with-long-name`
  const created = await request.post('/api/v1/definitions', { headers, data: {
    dsl: '1.0', name, inputs: [{ name: 'document', type: 'string', required: true }],
    nodes: [{ id: 'compliance_review_gate', type: 'approval', approvers: ['alice'], mode: 'any',
      instructions: 'UX-F1-a 自动化验收数据。请核对输入中的文档名称、审批范围与归档说明，再决定是否继续。\n备注将完整保留在决定时间线。',
      outputs: { decision: 'string' } }], edges: [],
  } })
  expect(created.ok()).toBe(true)
  const { id: draftId } = await created.json() as { id: string }
  const published = await request.post(`/api/v1/drafts/${draftId}/publish`, { headers })
  expect(published.ok()).toBe(true)
  const { version } = await published.json() as { version: number }
  const triggered = await request.post('/api/v1/executions', {
    headers: { ...headers, 'Idempotency-Key': `${name}-trigger` },
    data: { definition_name: name, version, input: { document: '自动化验收文档；无需调用模型' } },
  })
  expect(triggered.ok()).toBe(true)
  const { id } = await triggered.json() as { id: string }
  await expect.poll(async () => {
    const read = await request.get(`/api/v1/executions/${id}`, { headers })
    expect(read.ok()).toBe(true)
    return (await read.json() as { nodes: { node_id: string; status: string }[] }).nodes.find((n) => n.node_id === 'compliance_review_gate')?.status
  }, { timeout: 30_000 }).toBe('WAITING_APPROVAL')
  return { id, name, node: 'compliance_review_gate' }
}

// 由浏览器实际合成背景采样，支持 color-mix 与祖先透明背景；非禁用正文逐项断言。
async function contrast(page: Page) {
  return page.evaluate(() => {
    const canvas = document.createElement('canvas')
    canvas.width = canvas.height = 1
    const ctx = canvas.getContext('2d')!
    const rgb = (color: string) => {
      ctx.clearRect(0, 0, 1, 1); ctx.fillStyle = '#ffffff'; ctx.fillRect(0, 0, 1, 1)
      ctx.fillStyle = color; ctx.fillRect(0, 0, 1, 1)
      return [...ctx.getImageData(0, 0, 1, 1).data].slice(0, 3)
    }
    const background = (element: Element) => {
      const colors: string[] = []
      for (let current: Element | null = element; current; current = current.parentElement) colors.push(getComputedStyle(current).backgroundColor)
      ctx.clearRect(0, 0, 1, 1); ctx.fillStyle = '#ffffff'; ctx.fillRect(0, 0, 1, 1)
      for (const color of colors.reverse()) { ctx.fillStyle = color; ctx.fillRect(0, 0, 1, 1) }
      return [...ctx.getImageData(0, 0, 1, 1).data].slice(0, 3)
    }
    const luminance = (value: number[]) => {
      const v = value.map((c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 })
      return v[0] * 0.2126 + v[1] * 0.7152 + v[2] * 0.0722
    }
    const ratio = (a: number[], b: number[]) => {
      const values = [luminance(a), luminance(b)].sort((a, b) => b - a)
      return (values[0] + 0.05) / (values[1] + 0.05)
    }
    const text: { label: string; ratio: number }[] = []
    for (const element of document.body.querySelectorAll('*')) {
      if (element.closest('svg,script,style,[disabled],[aria-disabled="true"]')) continue
      const rect = element.getBoundingClientRect()
      const style = getComputedStyle(element)
      if (rect.width < 2 || rect.height < 2 || style.visibility !== 'visible' || style.display === 'none') continue
      const label = [...element.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.textContent?.trim() ?? '').join('')
      if (label) text.push({ label: label.slice(0, 60), ratio: ratio(rgb(style.color), background(element)) })
      if (element instanceof HTMLTextAreaElement || element instanceof HTMLInputElement) {
        text.push({ label: element.getAttribute('aria-label') ?? element.tagName, ratio: ratio(rgb(style.color), background(element)) })
      }
    }
    const controls = [...document.querySelectorAll<HTMLButtonElement | HTMLTextAreaElement>('.appr-detail .textarea,.appr-rail .panel-head .btn,.appr-detail .btn-primary')]
      .filter((el) => !el.disabled).map((el) => {
        const style = getComputedStyle(el)
        return { label: el.tagName + ' ' + el.textContent?.trim().slice(0, 20),
          inner: ratio(rgb(style.borderTopColor), background(el)), outer: ratio(rgb(style.borderTopColor), background(el.parentElement!)),
          fill: ratio(background(el), background(el.parentElement!)) }
      })
    const focused = document.activeElement
    let focus: { width: string; style: string; offset: string; ratio: number; textRatio: number; color: string; ground: number[]; composite: number[] } | null = null
    if (focused instanceof HTMLElement && focused.matches(':focus-visible')) {
      const style = getComputedStyle(focused)
      // outline 含透明度：同样先落到相邻真实背景再取样。
      const ground = background(focused)
      ctx.fillStyle = `rgb(${ground.join(',')})`; ctx.fillRect(0, 0, 1, 1)
      ctx.fillStyle = style.outlineColor; ctx.fillRect(0, 0, 1, 1)
      const outline = [...ctx.getImageData(0, 0, 1, 1).data].slice(0, 3)
      focus = { width: style.outlineWidth, style: style.outlineStyle, offset: style.outlineOffset,
        ratio: ratio(outline, ground), textRatio: ratio(rgb(style.color), ground), color: style.outlineColor, ground, composite: outline }
    }
    return { text, controls, focus, accent: getComputedStyle(document.documentElement).getPropertyValue('--color-accent').trim() }
  })
}

async function capture(page: Page, info: TestInfo, label: string) {
  await expect(page.locator('h1')).toHaveCount(1)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  const path = info.outputPath(`${label}.png`)
  // 先停稳动效并截屏，失败时也保留实际画面与原始对比度读数。
  await page.screenshot({ path, fullPage: true, animations: 'disabled' })
  await info.attach(label, { path, contentType: 'image/png' })
  const measured = await contrast(page)
  await info.attach(`${label}-contrast`, { body: JSON.stringify(measured, null, 2), contentType: 'application/json' })
  expect(measured.text.length).toBeGreaterThan(12)
  expect(measured.text.filter((item) => item.ratio < 4.5)).toEqual([])
  expect(measured.controls.filter((item) => item.outer < 3 || (item.inner < 3 && item.fill < 3))).toEqual([])
  if (measured.focus !== null) {
    expect(measured.focus.style).toBe('solid')
    expect(parseFloat(measured.focus.width)).toBeGreaterThanOrEqual(2)
    expect(measured.focus.ratio).toBeGreaterThanOrEqual(3)
  }
}

test('壳层与审批在两主题/桌面窄窗保持层级、对比度和键盘决定流', async ({ page, request }, info) => {
  await page.emulateMedia({ reducedMotion: 'reduce' })
  const item = await seedApproval(request)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/login')
  await page.getByTestId(TID.loginUsername).fill('alice')
  await page.getByTestId(TID.loginPassword).fill('e2e-alice-pass-1')
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.whoami)).toContainText('alice')
  for (const [nav, title] of [[TID.navWorkflows, '工作流'], [TID.navExecutions, '执行'], [TID.navApprovals, '审批']] as const) {
    await page.getByTestId(nav).click()
    await expect(page.getByRole('heading', { level: 1, name: title, exact: true })).toBeVisible()
    await expect(page.getByRole('navigation', { name: '主域导航' }).locator('[aria-current="page"]')).toHaveText(title)
  }
  const row = page.getByTestId(tid.approvalRow(item.id, item.node))
  for (const theme of ['dark', 'light'] as const) {
    await page.getByTestId(tid.themeOption(theme)).click()
    for (const width of [1440, 1024]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(row).toBeVisible()
      await expect(page.getByLabel('队列快捷键')).toBeVisible()
      await expect(page.getByLabel('队列快捷键')).not.toContainText('批准')
      expect(await row.evaluate((el) => el.getBoundingClientRect().height)).toBeLessThanOrEqual(48)
      await row.focus()
      await page.keyboard.press('ArrowDown') // 真正键盘输入使焦点环进入 focus-visible。
      await expect(row).toBeFocused()
      const queueFocus = await contrast(page)
      expect(queueFocus.focus).not.toBeNull()
      await capture(page, info, `${theme}-${width}-queue`)
      await page.keyboard.press('Enter')
      await expect(page.getByTestId(TID.approvalDetail)).toBeVisible()
      await expect(page.getByTestId(TID.approvalInstructions)).toContainText('UX-F1-a 自动化验收数据')
      await expect(page.getByLabel('详情快捷键')).toContainText('a 批准')
      await page.getByTestId(TID.approvalComment).focus()
      await page.keyboard.press('ArrowRight')
      await capture(page, info, `${theme}-${width}-detail`)
      await page.getByRole('button', { name: '返回队列', exact: true }).focus()
      await page.keyboard.press('Escape')
      await expect(row).toBeFocused()
    }
  }
  await page.keyboard.press('Enter')
  await expect(page.getByTestId(TID.approvalApprove)).toBeVisible()
  await page.getByTestId(TID.approvalReject).focus()
  await page.keyboard.press('r')
  await expect(page.getByRole('group', { name: '确认驳回', exact: true })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.approvalReject)).toBeFocused()
  await page.getByTestId(TID.approvalComment).fill('UX-F1-a 真实审批决定流验收')
  await page.getByRole('button', { name: '返回队列', exact: true }).focus()
  await page.keyboard.press('a')
  await expect(page.getByTestId(TID.approvalQueue)).toBeVisible()
  await expect(page.getByTestId(TID.approvalFlash)).toContainText('批准')
  await expect(row).toHaveCount(0)
  await info.attach('real-approval', { body: JSON.stringify(item), contentType: 'application/json' })
})

test('作者触发面Select的真实ArrowDown焦点在两主题桌面窄窗都有独有内侧环', async ({ page, request }, info) => {
  const login = await request.post('/api/v1/auth/login', { data: { username: 'author', password: 'e2e-author-pass-1' } })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const stamp = Date.now()
  for (const suffix of ['first', 'second']) {
    const created = await request.post('/api/v1/definitions', { headers, data: {
      dsl: '1.0', name: `ux_f1_a_select_${stamp}_${suffix}`,
      nodes: [{ id: 'gate', type: 'approval', approvers: ['alice'], mode: 'any', instructions: 'UX-F1-a Select 自动化验收定义。', outputs: { decision: 'string' } }], edges: [],
    } })
    expect(created.ok()).toBe(true)
    const { id } = await created.json() as { id: string }
    expect((await request.post(`/api/v1/drafts/${id}/publish`, { headers })).ok()).toBe(true)
  }
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/login')
  await page.getByTestId(TID.loginUsername).fill('author')
  await page.getByTestId(TID.loginPassword).fill('e2e-author-pass-1')
  await page.getByTestId(TID.loginSubmit).click()
  await page.getByTestId(TID.navExecutions).click()
  const trigger = page.getByTestId(TID.triggerDefinition)
  for (const theme of ['dark', 'light'] as const) {
    await page.getByTestId(tid.themeOption(theme)).click()
    for (const width of [1440, 1024]) {
      await page.setViewportSize({ width, height: 1000 })
      await trigger.click()
      const enabled = page.locator('[data-slot="select-item"]:not([data-disabled])')
      await expect.poll(() => enabled.count()).toBeGreaterThanOrEqual(2)
      await enabled.first().hover()
      await page.keyboard.press('Home')
      await page.keyboard.press('ArrowDown')
      const focused = page.locator('[data-slot="select-item"]:focus-visible')
      await expect(focused).toHaveCount(1)
      await expect(focused).toBeFocused()
      expect(await focused.evaluate((el) => el.matches(':hover'))).toBe(false)
      const label = `${theme}-${width}-select-keyboard`
      const path = info.outputPath(`${label}.png`)
      await page.screenshot({ path, fullPage: true, animations: 'disabled' })
      await info.attach(label, { path, contentType: 'image/png' })
      const measured = await contrast(page)
      await info.attach(`${label}-contrast`, { body: JSON.stringify(measured, null, 2), contentType: 'application/json' })
      expect(measured.focus).not.toBeNull()
      expect(measured.focus?.style).toBe('solid')
      expect(parseFloat(measured.focus?.width ?? '0')).toBeGreaterThanOrEqual(2)
      expect(parseFloat(measured.focus?.offset ?? '0')).toBeLessThan(0)
      expect(measured.focus?.ratio).toBeGreaterThanOrEqual(3)
      expect(measured.focus?.textRatio).toBeGreaterThanOrEqual(4.5)
      await page.keyboard.press('Escape')
      await expect(trigger).toBeFocused()
    }
  }
})
