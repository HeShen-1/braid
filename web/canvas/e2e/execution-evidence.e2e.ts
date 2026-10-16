// T23.9：真实浏览器→API→worker→测试内免费OpenAI兼容桩→台账/审计。
// usage 来自工程桩响应并由后端持久化；gpt-x 使用 e2e 既有0 USD价表。
// 此证据不代表真实模型结果或真实供应商费用。测试不拦截浏览器请求。
import { expect, test } from '@playwright/test'
import { createServer } from 'node:http'
import path from 'node:path'
import type { AuditEvent, NodeAttempt, NodeProjection } from '../src/api.ts'
import type { WorkflowDoc } from '../src/model/dsl.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

test('完整证据：100+审计游标可达，回滚前后按轮核对输入/输出/usage/trace', async ({ page, request }, testInfo) => {
  test.setTimeout(240_000)
  const calls: { key: string; model: string; messages: unknown }[] = []
  const stubErrors: string[] = []
  const server = createServer(async (req, res) => {
    try {
      const parts: Buffer[] = []
      for await (const part of req) parts.push(Buffer.from(part))
      const body = JSON.parse(Buffer.concat(parts).toString()) as { model: string; messages: unknown }
      const key = String(req.headers['idempotency-key'] ?? '')
      if (req.method !== 'POST' || req.url !== '/v1/chat/completions' || !/^att:\d+:\d+$/.test(key) || body.model !== 'gpt-x') {
        throw new Error(`unexpected engineering request: ${req.method} ${req.url}, model=${body.model}, key=${key}`)
      }
      calls.push({ key, model: body.model, messages: body.messages })
      const round = Number(key.split(':').at(-1))
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({
        id: `engineering-${round}`, object: 'chat.completion', model: 'gpt-x',
        choices: [{ index: 0, message: { role: 'assistant', content: JSON.stringify({ text: `engineering-round-${round}` }) }, finish_reason: 'stop' }],
        usage: { prompt_tokens: round * 17, completion_tokens: round * 5, total_tokens: round * 22 },
      }))
    } catch (err) {
      stubErrors.push(String(err))
      res.writeHead(400, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ error: { message: String(err), type: 'engineering_fixture_error' } }))
    }
  })

  try {
    // EADDRINUSE 响亮失败，不接管已有监听者。整个生命周期在 finally 关闭。
    await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(18082, '127.0.0.1', resolve) })
    const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
    expect(login.ok()).toBe(true)
    const { token } = await login.json() as { token: string }
    const headers = { Authorization: `Bearer ${token}` }
    const doc: WorkflowDoc = {
      dsl: '1.0', name: `t239-evidence-${Date.now()}`,
      description: '免费工程桩 usage 落库及审计游标验收',
      inputs: [{ name: 'text', type: 'string', required: true }, { name: 'docs', type: 'array', required: true }],
      nodes: [
        { id: 'model', type: 'llm', model: 'gpt-x', max_tokens: 64, response_format: 'json', prompt: { template: 'Return JSON with text.', user_data: [{ name: 'source', ref: '{{inputs.text}}' }] }, outputs: { text: 'string' } },
        { id: 'fan', type: 'parallel', parallel: { foreach: '{{inputs.docs}}', on_failure: 'wait_all', body: { type: 'tool', tool: 'file_read', inputs: { path: '{{item}}' }, outputs: { content: 'string' } } } },
      ],
      edges: [{ from: 'model', to: 'fan' }], outputs: { text: '{{nodes.model.output.text}}' },
    }
    const created = await request.post('/api/v1/definitions', { headers, data: doc })
    expect(created.status(), await created.text()).toBe(201)
    const { id } = await created.json() as { id: string }
    const published = await request.post(`/api/v1/drafts/${id}/publish`, { headers })
    expect(published.ok(), await published.text()).toBe(true)
    const { version } = await published.json() as { version: number }
    const input = { text: 't239-saved-input', docs: Array.from({ length: 36 }, () => path.resolve(process.cwd(), 'e2e/fixtures/files/a.txt')) }

    await page.goto('/executions')
    await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
    await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
    await page.getByTestId(TID.loginSubmit).click()
    await page.getByTestId(TID.triggerDefinition).click()
    await page.getByRole('option', { name: `${doc.name} v${version}`, exact: true }).click()
    await page.getByTestId(TID.triggerInput).fill(JSON.stringify(input))
    const triggered = page.waitForResponse((res) => res.request().method() === 'POST' && new URL(res.url()).pathname === '/api/v1/executions')
    await page.getByTestId(TID.triggerSubmit).click()
    const response = await triggered
    expect(response.status()).toBe(201)
    const { id: execId } = await response.json() as { id: string }
    await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 60_000 })
    await expect(page.getByTestId(TID.execInput)).toContainText(input.text)
    await expect(page.getByTestId(TID.execDetail)).toContainText('执行墙钟（含跨轮间隔）')
    await expect(page.getByTestId(TID.execRibbon)).toContainText('跨轮墙钟')

    const readRound = async (round: number): Promise<NodeAttempt> => {
      const result = await request.get(`/api/v1/executions/${execId}/nodes/model/attempts/${round}?instance_no=0`, { headers })
      expect(result.ok()).toBe(true)
      return result.json() as Promise<NodeAttempt>
    }
    const first = await readRound(1)
    expect(first).toMatchObject({ round: 1, status: 'SUCCEEDED', model: 'gpt-x', prompt_tokens: 17, completion_tokens: 5, total_tokens: 22, cost_amount: 0, cost_currency: 'USD', estimated: false, output: { text: 'engineering-round-1' } })
    expect(JSON.stringify(first.input_snapshot)).toContain(input.text)

    // 独立证据入口不会选中/改变回滚目标。
    const nodeOpener = page.getByTestId(tid.nodeEvidence('model', 0))
    const nodePanel = page.getByTestId(TID.nodeEvidence)
    await nodeOpener.click()
    await expect(nodePanel).toBeFocused()
    await expect(nodePanel.getByRole('heading', { name: '节点证据', exact: true })).toBeInViewport()
    await page.getByRole('button', { name: '收起证据' }).click()
    await expect(nodeOpener).toBeFocused()
    await expect(nodeOpener).toBeInViewport()
    await nodeOpener.press('Enter')
    await expect(nodePanel).toBeFocused()
    await expect(nodePanel.getByRole('heading', { name: '节点证据', exact: true })).toBeInViewport()
    await expect(page.getByTestId(TID.rollbackSubmit)).toBeDisabled()
    const evidence = page.getByTestId(TID.attemptEvidence)
    await expect(evidence).toContainText('engineering-round-1')
    await expect(evidence).toContainText(input.text)
    await expect(evidence).toContainText('输入 17 · 输出 5 · 总计 22 tokens')
    await expect(evidence).toContainText('成本 0 USD')
    await expect(evidence).toContainText(first.attempt_id)

    // 正常工具实例产生审计，直接读真实API对拍全量，不人工插audit_log。
    const readAudit = async (): Promise<AuditEvent[]> => {
      const rows: AuditEvent[] = []
      let cursor = ''
      do {
        const result = await request.get(`/api/v1/audit?execution_id=${execId}&limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, { headers })
        expect(result.ok()).toBe(true)
        const body = await result.json() as { items: AuditEvent[]; next_cursor: string }
        rows.push(...body.items); cursor = body.next_cursor
      } while (cursor)
      return rows
    }
    await expect.poll(async () => (await readAudit()).length, { timeout: 30_000 }).toBeGreaterThan(100)
    const auditRefresh = page.getByRole('button', { name: '刷新审计', exact: true })
    await auditRefresh.click()
    await expect(page.getByTestId(TID.auditCount)).toContainText('已加载 100 条 · 仍有更多')
    let allAudit: AuditEvent[] = []
    // >100和SUCCEEDED都不代表audit_pending排空：有界等待本次API/UI集合一致。
    await expect(async () => {
      allAudit = await readAudit()
      expect(allAudit.length).toBeGreaterThan(100)
      await auditRefresh.click()
      await expect(auditRefresh).toBeEnabled()
      while (await page.getByTestId(TID.auditMore).count()) {
        await page.getByTestId(TID.auditMore).click()
        await expect(auditRefresh).toBeEnabled()
      }
      const displayedIds = await page.getByTestId(TID.auditList).locator('tbody tr[data-event-id]').evaluateAll((rows) => rows.map((row) => row.getAttribute('data-event-id')))
      expect(displayedIds.sort()).toEqual(allAudit.map((row) => row.id).sort())
      await expect(page.getByTestId(TID.auditCount)).toContainText(`已加载 ${allAudit.length} 条 · 已到末页`)
    }).toPass({ timeout: 30_000, intervals: [500, 1000] })
    const rawAuditPages = page.locator('details').filter({ has: page.locator('summary', { hasText: /^审计第 \d+ 页服务端响应原文$/ }) })
    const rawAuditText = (await rawAuditPages.locator('pre').allTextContents()).join('\n')
    expect(rawAuditText).toMatch(new RegExp(`"definition_id"\\s*:\\s*${id}([,}])`))
    const modelAudit = allAudit.find((row) => row.event_type === 'llm.call.completed' && row.attempt_id === first.attempt_id)!
    expect(modelAudit).toMatchObject({ prompt_tokens: 17, completion_tokens: 5, total_tokens: 22, cost_amount: 0, cost_currency: 'USD', cost_estimated: false })
    const auditRow = page.getByTestId(tid.auditRow('llm.call.completed'))
    await auditRow.locator('summary').click()
    await expect(auditRow).toContainText(modelAudit.row_hash!)
    await expect(auditRow).toContainText('payload（解析后）')
    await expect(auditRow).toContainText('成本 0 USD')
    expect(modelAudit.trace_id).toMatch(/^[0-9a-f]{32}$/)
    const traceOpener = auditRow.getByRole('button', { name: /查询 trace/ })
    await traceOpener.click()
    const traceTable = page.getByTestId(TID.traceAuditList)
    const tracePanel = traceTable.locator('..').locator('..')
    const traceRefresh = tracePanel.getByRole('button', { name: '刷新审计', exact: true })
    await expect(tracePanel).toBeFocused()
    await expect(tracePanel.getByRole('heading', { name: 'Trace 审计', exact: true })).toBeInViewport()
    await expect(traceRefresh).toBeEnabled()
    while (await tracePanel.getByRole('button', { name: '加载更多审计', exact: true }).count()) {
      await tracePanel.getByRole('button', { name: '加载更多审计', exact: true }).click()
      await expect(traceRefresh).toBeEnabled()
    }
    await expect(traceTable).toContainText(first.attempt_id)
    await page.getByRole('button', { name: '收起 Trace' }).click()
    await expect(traceOpener).toBeFocused()
    await expect(traceOpener).toBeInViewport()
    await traceOpener.press('Enter')
    await expect(page.getByRole('region', { name: 'Trace 审计' })).toBeFocused()
    await expect(page.getByRole('region', { name: 'Trace 审计' }).getByRole('heading', { name: 'Trace 审计' })).toBeInViewport()
    await page.getByRole('button', { name: '收起 Trace' }).click()

    // 原回滚漏斗：时间带选点→布防→显式确认→真实后端重跑。
    await page.getByTestId(tid.nodeRow('model', 0)).locator('button.ribbon-label').click()
    await page.getByTestId(TID.rollbackSubmit).click()
    expect(calls).toHaveLength(1)
    await page.getByTestId(TID.rollbackConfirm).click()
    await expect(page.getByTestId(TID.execFlash)).toBeVisible()
    await expect.poll(async () => calls.length, { timeout: 30_000 }).toBe(2)
    await expect(page.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 60_000 })
    await page.getByRole('button', { name: '刷新证据', exact: true }).click()
    await expect(page.getByTestId(tid.attemptRow(2))).toContainText('当前轮代')
    await expect(page.getByTestId(tid.attemptRow(1))).toContainText('历史轮次')
    await page.getByRole('button', { name: '第 2 轮', exact: true }).click()
    await expect(evidence).toContainText('engineering-round-2')
    await expect(evidence).toContainText('输入 34 · 输出 10 · 总计 44 tokens')
    await expect(evidence).toContainText(input.text)
    const second = await readRound(2)
    expect(second).toMatchObject({ round: 2, status: 'SUCCEEDED', model: 'gpt-x', prompt_tokens: 34, completion_tokens: 10, total_tokens: 44, cost_amount: 0, cost_currency: 'USD', estimated: false, output: { text: 'engineering-round-2' } })
    expect(second.input_snapshot).toEqual(first.input_snapshot)
    expect(second.attempt_id).not.toBe(first.attempt_id)
    await expect(evidence).toContainText(second.attempt_id)
    await page.getByRole('button', { name: '第 1 轮', exact: true }).click()
    await expect(evidence).toContainText('engineering-round-1')
    await expect(evidence).toContainText('输入 17 · 输出 5 · 总计 22 tokens')
    await expect(evidence).not.toContainText('engineering-round-2')
    expect(await readRound(1)).toEqual(first)
    const projectionResponse = await request.get(`/api/v1/executions/${execId}/nodes/model?instance_no=0`, { headers })
    const projection = await projectionResponse.json() as NodeProjection
    expect(projection).toMatchObject({ attempts_base: 1, attempts: 2, output: second.output })
    expect(stubErrors).toEqual([])
    expect(calls.map((call) => call.key)).toEqual([`att:${projection.node_execution_id}:1`, `att:${projection.node_execution_id}:2`])
    expect(calls.every((call) => JSON.stringify(call.messages).includes(input.text))).toBe(true)

    await testInfo.attach('persisted-engineering-evidence', { body: JSON.stringify({ execId, input, first, second, projection, allAudit, calls }, null, 2), contentType: 'application/json' })
    await page.getByTestId(tid.themeOption('dark')).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
    await page.getByTestId(TID.nodeEvidence).screenshot({ path: testInfo.outputPath('node-rounds-dark.png'), animations: 'disabled' })
    await page.getByTestId(tid.themeOption('light')).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
    await page.getByTestId(TID.nodeEvidence).screenshot({ path: testInfo.outputPath('node-rounds-light.png'), animations: 'disabled' })
    await page.getByTestId(TID.auditList).locator('thead').scrollIntoViewIfNeeded()
    await page.screenshot({ path: testInfo.outputPath('audit-light.png'), animations: 'disabled' })
  } finally {
    server.closeAllConnections()
    await new Promise<void>((resolve, reject) => server.close((err) => err && 'code' in err && err.code !== 'ERR_SERVER_NOT_RUNNING' ? reject(err) : resolve()))
  }
})
