// T21.4 ④ 画布节点删除的浏览器级验收(键盘 Delete 与可见入口走同一条确认漏斗)。
// 与 rollback-journey.e2e.ts 的分工:那边是执行/审批旅程,这边只钉画布编辑面
// 的新交互 —— 入口可见 → 第一下只布防 → 确认后节点与 DSL 预览同步消失 → 键盘
// 快捷键同路。选择器单一真源 = src/ui/testids.ts。
import { expect, test } from '@playwright/test'
import type { WorkflowDoc } from '../src/model/dsl.ts'
import { loadBase } from '../src/model/testdata.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

test('画布节点删除:文字入口 + 确认层;Delete/Backspace 同路;取消保留节点', async ({ page }) => {
  // ① 登录(author 有画布编辑权;e2e 栈 seed 见 deploy/e2e-up.sh)
  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()

  // ② 新建本地工作流 → 空白画布 → 加一个 branch 节点(多锚点那一类)
  await page.goto('/workflows')
  await page.getByTestId(TID.draftNew).click()
  await expect(page.getByTestId(TID.editor)).toBeVisible()
  await page.getByTestId(tid.paletteBtn('branch')).click()
  const branchCard = page.getByTestId(tid.nodeCard('branch'))
  await expect(branchCard).toBeVisible()

  // ③ 未选中节点:没有删除入口(入口挂在属性面板上,不虚设)
  await expect(page.getByTestId(TID.nodeDelete)).toHaveCount(0)

  // ④ 选中节点 → 可见入口在场 → 第一下只布防(危险动作第二下才生效)
  await branchCard.click()
  const deleteEntry = page.getByTestId(TID.nodeDelete)
  await expect(deleteEntry).toHaveText('删除节点')
  await expect(deleteEntry).toHaveAttribute('aria-label', '删除节点')
  await expect(deleteEntry).toHaveAttribute('title', '删除节点 branch')
  await deleteEntry.click()
  await expect(page.getByTestId(TID.deleteConfirm)).toBeVisible()
  await expect(branchCard).toBeVisible()
  await expect(page.getByTestId(TID.editorJson)).toContainText('branch')
  await page.getByRole('button', { name: '放弃', exact: true }).click()
  await expect(page.getByTestId(TID.deleteConfirm)).toHaveCount(0)
  await expect(deleteEntry).toBeFocused()
  await expect(branchCard).toBeVisible()
  await expect(page.getByTestId(TID.editorJson)).toContainText('"branch"')

  // ⑤ 确认 → 节点从画布与 DSL 预览一起消失
  await deleteEntry.press('Enter')
  await expect(page.getByTestId(TID.deleteConfirm)).toBeVisible()
  await page.getByTestId(TID.deleteConfirmSubmit).click()
  await expect(page.getByTestId(TID.deleteConfirm)).toHaveCount(0)
  await expect(page.getByTestId(tid.nodeCard('branch'))).toHaveCount(0)
  await expect(page.getByTestId(TID.editorJson)).not.toContainText('"branch"')

  // ⑥ 键盘 Delete 走同一条漏斗:先布防(Esc 收起后焦点回入口按钮),再确认才删
  await page.getByTestId(tid.paletteBtn('llm')).click()
  const llmCard = page.getByTestId(tid.nodeCard('llm'))
  await expect(llmCard).toBeVisible()
  await llmCard.click()
  await page.keyboard.press('Delete')
  await expect(page.getByTestId(TID.deleteConfirm)).toBeVisible()
  await expect(llmCard).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId(TID.deleteConfirm)).toHaveCount(0)
  await expect(page.getByTestId(TID.nodeDelete)).toBeFocused()
  await expect(llmCard).toBeVisible()

  await page.keyboard.press('Backspace')
  await expect(page.getByTestId(TID.deleteConfirm)).toBeVisible()
  await expect(llmCard).toBeVisible()
  await page.getByTestId(TID.deleteConfirmSubmit).click()
  await expect(page.getByTestId(tid.nodeCard('llm'))).toHaveCount(0)
})

test('已连接分支删除:放弃保持 DSL;确认后节点、连线、锚点记录一起移除', async ({ page, request }) => {
  const login = await request.post('/api/v1/auth/login', { data: AUTHOR })
  expect(login.ok()).toBe(true)
  const { token } = await login.json() as { token: string }
  const headers = { Authorization: `Bearer ${token}` }
  const doc = { ...loadBase('with-positions'), name: `ux-f3-delete-${Date.now()}` }
  // 存储视口覆盖整幅既有图,不依赖浏览器默认窗口宽度或测试内平移。
  doc.metadata!.canvas!.viewport = { x: 0, y: 0, zoom: 0.35 }
  const created = await request.post('/api/v1/definitions', { headers, data: doc })
  expect(created.ok()).toBe(true)
  const { id } = await created.json() as { id: string }

  await page.goto('/')
  await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
  await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
  await page.getByTestId(TID.loginSubmit).click()
  await expect(page.getByTestId(TID.navExecutions)).toBeVisible()
  await page.goto(`/workflows/${id}`)
  await expect(page.getByTestId(TID.editor)).toBeVisible()
  const branchCard = page.getByTestId(tid.nodeCard('verdict'))
  await branchCard.click()
  const before = JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}') as WorkflowDoc
  expect(before.metadata?.canvas?.edges).toHaveLength(2)
  await expect(page.locator('.react-flow__node[data-id="verdict"] .react-flow__handle')).not.toHaveCount(0)

  const deleteEntry = page.getByTestId(TID.nodeDelete)
  await expect(deleteEntry).toHaveText('删除节点')
  await deleteEntry.click()
  await expect(page.getByTestId(TID.deleteConfirm)).toContainText('删除节点 verdict')
  await page.getByRole('button', { name: '放弃', exact: true }).click()
  await expect(deleteEntry).toBeFocused()
  expect(JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}')).toEqual(before)

  await deleteEntry.click()
  await page.getByTestId(TID.deleteConfirmSubmit).click()
  await expect(branchCard).toHaveCount(0)
  await expect(page.locator('.react-flow__node[data-id="verdict"] .react-flow__handle')).toHaveCount(0)
  await expect(page.locator('.react-flow__edge[data-id*="verdict"]')).toHaveCount(0)
  const after = JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}') as WorkflowDoc
  expect(after.nodes).toEqual(before.nodes.filter((n) => n.id !== 'verdict'))
  expect(after.edges).toEqual(before.edges?.filter((e) => e.from !== 'verdict' && e.to !== 'verdict'))
  expect(after.metadata?.canvas?.nodes).not.toHaveProperty('verdict')
  expect(after.metadata?.canvas?.edges ?? []).toEqual([])
})
