import { expect, test, type Page } from '@playwright/test'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }
const ALICE = { username: 'alice', password: 'e2e-alice-pass-1' }
const BOB = { username: 'bob', password: 'e2e-bob-pass-1' }

async function login(page: Page, user: typeof AUTHOR) {
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(user.username)
  await page.getByTestId(TID.loginPassword).fill(user.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navWorkflows)).toBeVisible()
}

async function selectFlow(page: Page, name: string): Promise<string> {
  await page.goto('/executions')
  await page.getByTestId(TID.triggerDefinition).click()
  await page.getByRole('option', { name: `${name} v1`, exact: true }).click()
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/executions'),
    page.getByTestId(TID.triggerSubmit).click(),
  ])
  expect(response.status()).toBe(201)
  return (await response.json() as { id: string }).id
}

async function openApproval(page: Page, id: string, node: string) {
  await page.goto('/approvals')
  const row = page.getByTestId(tid.approvalRow(id, node))
  await expect(row).toBeVisible({ timeout: 30_000 })
  await row.click()
  await expect(page.getByTestId(TID.approvalDetail)).toHaveAttribute('data-state', 'WAITING_APPROVAL')
}

async function decide(page: Page, comment: string, kind: 'approve' | 'reject') {
  await page.getByTestId(TID.approvalComment).fill(comment)
  await page.getByTestId(kind === 'approve' ? TID.approvalApprove : TID.approvalReject).click()
  if (kind === 'reject') await page.getByRole('button', { name: '确认驳回' }).click()
  await expect(page.getByTestId(TID.approvalFlash)).toBeVisible({ timeout: 30_000 })
}

test('空白画布三节点及多人审批，窄屏完整记录和驳回', async ({ browser }, testInfo) => {
  test.setTimeout(240_000)
  const contexts = await Promise.all([browser.newContext({ viewport: { width: 1440, height: 900 } }), browser.newContext({ viewport: { width: 1440, height: 900 } }), browser.newContext({ viewport: { width: 1440, height: 900 } })])
  const [author, alice, bob] = await Promise.all(contexts.map((ctx) => ctx.newPage()))
  try {
    await Promise.all([login(author, AUTHOR), login(alice, ALICE), login(bob, BOB)])
    const name = `qa-t243-${Date.now()}`
    await author.getByTestId(TID.draftNew).click()
    await expect(author.getByText('空白画布', { exact: true })).toBeVisible()
    await author.getByRole('button', { name: '工作流设置', exact: true }).click()
    await author.getByLabel('名称', { exact: true }).fill(name)
    for (const [index, title, people] of [[0, '双人会签', 'alice, bob'], [1, '复核审批', 'bob'], [2, '最终审批', 'alice']] as const) {
      if (!await author.locator('.editor-palette').evaluate((el) => (el as HTMLDetailsElement).open)) await author.getByText('添加节点', { exact: true }).click()
      await author.getByTestId(tid.paletteBtn('approval')).click()
      const id = index === 0 ? 'approval' : `approval_n${index + 1}`
      await expect(author.getByTestId(tid.nodeCard(id))).toBeVisible()
      await author.getByLabel('标题', { exact: true }).fill(title)
      await author.getByLabel('审批人(逗号分隔)', { exact: true }).fill(people)
      if (index === 0) {
        await author.getByLabel('签署模式', { exact: true }).click()
        await author.getByRole('option', { name: 'all · 会签(全部批准才过,任一驳回即败)', exact: true }).click()
      }
    }
    const box = async (id: string) => (await author.getByTestId(tid.nodeCard(id)).boundingBox())!
    await expect.poll(async () => {
      const [a, b, c] = await Promise.all(['approval', 'approval_n2', 'approval_n3'].map(box))
      return !!a && !!b && !!c && a.y + a.height <= b.y + 1 && b.y + b.height <= c.y + 1
    }, { message: '连续创建的三个节点由上至下且不重叠' }).toBe(true)
    await author.screenshot({ path: testInfo.outputPath('canvas-desktop.png') })
    const handle = (id: string, type: 'source' | 'target', side: string) => author.getByTestId(tid.nodeCard(id)).locator(`.react-flow__handle.${type}[data-side="${side}"]`)
    await handle('approval', 'source', 'bottom').dragTo(handle('approval_n2', 'target', 'top'))
    await handle('approval_n2', 'source', 'bottom').dragTo(handle('approval_n3', 'target', 'top'))
    await expect(author.locator('.react-flow__edge')).toHaveCount(2)
    await author.getByTestId(TID.editorSave).click()
    await expect(author.getByTestId(TID.editorPublish)).toBeEnabled()
    await author.getByTestId(TID.editorPublish).click()
    await expect(author.getByTestId(TID.workflowFlash)).toContainText(`已发布 ${name} v1`)

    const id = await selectFlow(author, name)
    await openApproval(alice, id, 'approval')
    const longComment = '材料核对完成'.repeat(35)
    await decide(alice, longComment, 'approve')
    await openApproval(bob, id, 'approval')
    await bob.getByRole('tab', { name: '审批记录', exact: true }).click()
    const timeline = bob.getByTestId(TID.approvalTimeline)
    await expect(timeline).toContainText(longComment)
    await expect(timeline.locator('thead')).toBeVisible()
    await bob.screenshot({ path: testInfo.outputPath('history-desktop.png') })
    await bob.setViewportSize({ width: 390, height: 844 })
    for (const state of ['expanded', 'collapsed'] as const) {
      if (state === 'collapsed') await bob.getByRole('button', { name: '收起侧栏' }).click()
      const metrics = await timeline.evaluate((table) => {
        const p = table.parentElement!
        const cells = [...table.querySelectorAll('tbody td')]
        return { containerWidth: p.clientWidth, scrollWidth: p.scrollWidth, cells: cells.map((el) => ({ label: el.getAttribute('data-label'), width: el.clientWidth, scroll: el.scrollWidth, before: getComputedStyle(el, '::before').content })) }
      })
      expect(metrics.scrollWidth).toBeLessThanOrEqual(metrics.containerWidth + 1)
      expect(metrics.cells.map((x) => x.label)).toEqual(['处理人', '决定', '处理意见', '时间', '轮次'])
      for (const cell of metrics.cells) {
        expect(cell.scroll).toBeLessThanOrEqual(cell.width + 1)
        expect(cell.before).not.toBe('none')
      }
      await bob.screenshot({ path: testInfo.outputPath(`history-narrow-${state}.png`) })
    }
    await bob.getByRole('button', { name: '展开侧栏' }).click()
    await bob.setViewportSize({ width: 1440, height: 900 })
    await decide(bob, 'B 会签通过', 'approve')
    await openApproval(bob, id, 'approval_n2')
    await decide(bob, 'B 复核通过', 'approve')
    await openApproval(alice, id, 'approval_n3')
    await expect(author.getByTestId(tid.nodeRow('approval_n3', 0))).toHaveAttribute('data-state', 'WAITING_APPROVAL', { timeout: 30_000 })
    await decide(alice, 'A 终审通过', 'approve')
    await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'SUCCEEDED', { timeout: 30_000 })

    const rejected = await selectFlow(author, name)
    await openApproval(alice, rejected, 'approval')
    await decide(alice, 'A 同意', 'approve')
    await openApproval(bob, rejected, 'approval')
    await decide(bob, 'B 驳回：材料需补充', 'reject')
    await expect(author.getByTestId(TID.execStatus)).toHaveAttribute('data-state', 'FAILED', { timeout: 30_000 })
    await expect(author.getByTestId(tid.nodeRow('approval_n3', 0))).toHaveAttribute('data-state', 'PENDING')
    await bob.getByRole('button', { name: '查看刚才的决定' }).click()
    await bob.getByRole('tab', { name: '审批记录', exact: true }).click()
    await expect(bob.getByTestId(TID.approvalTimeline)).toContainText('A 同意')
    await expect(bob.getByTestId(TID.approvalTimeline)).toContainText('B 驳回：材料需补充')
    await bob.screenshot({ path: testInfo.outputPath('rejected-history.png') })
  } finally {
    await Promise.all(contexts.map((ctx) => ctx.close()))
  }
})
