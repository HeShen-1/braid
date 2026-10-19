// UX-F2/#228:真实添加操作、选中联动与实际 DOM 几何;不预置坐标或移动节点。
// 被测面由正式 e2e 栈提供,本文件不改动浏览器动效、DOM 或画布内部状态。
import { expect, test, type Page, type TestInfo } from '@playwright/test'
import { writeFile } from 'node:fs/promises'
import type { NodeType, WorkflowDoc } from '../src/model/dsl.ts'
import { TID, tid } from '../src/ui/testids.ts'

const AUTHOR = { username: 'author', password: 'e2e-author-pass-1' }

type Box = { x: number; y: number; width: number; height: number }
type CanvasGeometry = {
  canvas: Box
  cards: (Box & { id: string; selected: boolean })[]
}

async function attachJson(testInfo: TestInfo, name: string, value: unknown) {
  const file = testInfo.outputPath(`${name}.json`)
  await writeFile(file, JSON.stringify(value, null, 2))
  await testInfo.attach(name, { path: file, contentType: 'application/json' })
}

async function canvasDoc(page: Page): Promise<WorkflowDoc> {
  return JSON.parse(await page.getByTestId(TID.editorJson).textContent() ?? '{}') as WorkflowDoc
}

async function geometry(page: Page): Promise<CanvasGeometry> {
  return page.locator('.editor-canvas').evaluate((canvas) => {
    const box = (el: Element) => {
      const { x, y, width, height } = el.getBoundingClientRect()
      return { x, y, width, height }
    }
    return {
      canvas: box(canvas),
      cards: Array.from(canvas.querySelectorAll('.react-flow__node')).map((node) => ({
        ...box(node.querySelector('.bn')!),
        id: node.getAttribute('data-id') ?? '',
        selected: node.classList.contains('selected'),
      })),
    }
  })
}

function geometryProblems(snapshot: CanvasGeometry, addedId: string): string[] {
  const problems: string[] = []
  const added = snapshot.cards.find((card) => card.id === addedId)
  if (added === undefined) return ['新卡片尚未呈现']
  const canvas = snapshot.canvas
  if (added.x < canvas.x - 0.5 || added.y < canvas.y - 0.5 ||
      added.x + added.width > canvas.x + canvas.width + 0.5 ||
      added.y + added.height > canvas.y + canvas.height + 0.5) problems.push(`${addedId} 未完整显示在画布内`)
  for (let i = 0; i < snapshot.cards.length; i++) {
    const a = snapshot.cards[i]
    if (a.width <= 0 || a.height <= 0) problems.push(`${a.id} 没有可见尺寸`)
    for (const b of snapshot.cards.slice(i + 1)) {
      if (Math.min(a.x + a.width, b.x + b.width) > Math.max(a.x, b.x) &&
          Math.min(a.y + a.height, b.y + b.height) > Math.max(a.y, b.y)) problems.push(`${a.id} 与 ${b.id} 重叠`)
    }
  }
  return problems
}

async function selectedPanel(page: Page, id: string) {
  await expect(page.getByTestId(TID.nodePanel)).toBeVisible()
  await expect(page.getByTestId(TID.nodePanel).locator('header code')).toHaveText(id)
  await expect(page.getByTestId(TID.nodeDelete)).toHaveAttribute('title', `删除节点 ${id}`)
}

async function addNode(
  page: Page,
  testInfo: TestInfo,
  type: NodeType,
  phase: string,
  activation: 'click' | 'Enter' | 'Space' = 'click',
  panelVisible = true,
): Promise<string> {
  const before = await canvasDoc(page)
  const button = page.getByTestId(tid.paletteBtn(type))
  if (activation === 'click') {
    await button.click()
  } else {
    // 焦点由上一按钮的 Tab 自然到达,不调用 focus() 代替键盘导航。
    await expect(button).toBeFocused()
    await expect.poll(() => button.evaluate((el) => {
      const style = getComputedStyle(el)
      return el.matches(':focus-visible') && style.outlineStyle !== 'none' && Number.parseFloat(style.outlineWidth) >= 2
    })).toBe(true)
    await page.keyboard.press(activation)
  }
  await expect(button).toBeFocused()
  await expect(page.locator('.react-flow__node')).toHaveCount(before.nodes.length + 1)
  const after = await canvasDoc(page)
  const added = after.nodes.filter((node) => !before.nodes.some((old) => old.id === node.id))
  expect(added).toHaveLength(1)
  const id = added[0].id
  expect(added[0].type).toBe(type)
  await expect(page.locator('.react-flow__node.selected')).toHaveCount(1)
  await expect(page.locator('.react-flow__node.selected')).toHaveAttribute('data-id', id)
  if (panelVisible) await selectedPanel(page, id)
  else await expect(page.getByTestId(TID.nodePanel)).toHaveCount(0)

  let previous = ''
  try {
    await expect.poll(async () => {
      const snapshot = await geometry(page)
      const key = JSON.stringify(snapshot)
      const problems = geometryProblems(snapshot, id)
      if (key !== previous) problems.push('等待实际布局稳定')
      previous = key
      return problems
    }, { message: `${phase}: 新卡片可见且全部卡片互不覆盖`, timeout: 10_000 }).toEqual([])
  } finally {
    await attachJson(testInfo, `${phase}-${id}-geometry`, { dsl: await canvasDoc(page), geometry: await geometry(page) })
  }
  // 新增不改变已有节点 spec/位置;视口为显露新卡片而缩放是允许的。
  const settled = await canvasDoc(page)
  expect(settled.nodes.filter((node) => node.id !== id)).toEqual(before.nodes)
  for (const old of before.nodes) {
    expect(settled.metadata?.canvas?.nodes?.[old.id]).toEqual(before.metadata?.canvas?.nodes?.[old.id])
  }
  return id
}

async function editSelectedTitle(page: Page, id: string) {
  const before = await canvasDoc(page)
  const title = `当前编辑 ${id}`
  await page.getByTestId(TID.nodePanel).getByLabel('标题', { exact: true }).fill(title)
  await expect.poll(async () => (await canvasDoc(page)).nodes.find((node) => node.id === id)?.title).toBe(title)
  const after = await canvasDoc(page)
  expect(after.nodes.filter((node) => node.id !== id)).toEqual(before.nodes.filter((node) => node.id !== id))
  await expect(page.getByTestId(tid.nodeCard(id))).toContainText(title)
}

async function screenshot(page: Page, testInfo: TestInfo, name: string) {
  const path = testInfo.outputPath(`${name}.png`)
  await page.screenshot({ path, fullPage: true, animations: 'allow' })
  await testInfo.attach(name, { path, contentType: 'image/png' })
}

for (const width of [1440, 1024]) {
  for (const theme of ['dark', 'light'] as const) {
    test(`新节点防堆叠与自动选中: ${width}px ${theme}, 键盘/折叠/长分支`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 })
      await page.goto('/')
      await page.getByTestId(TID.loginUsername).fill(AUTHOR.username)
      await page.getByTestId(TID.loginPassword).fill(AUTHOR.password)
      await page.getByTestId(TID.loginSubmit).click()
      await expect(page.getByTestId(TID.navWorkflows)).toBeVisible()
      await page.getByTestId(TID.navWorkflows).click()
      await page.getByTestId(tid.themeOption(theme)).click()
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
      await page.getByTestId(TID.draftNew).click()
      await expect(page.getByText('空白画布', { exact: true })).toBeVisible()

      // 四类卡片连续添加,中间不移卡片、不调坐标或画布视口。
      await addNode(page, testInfo, 'llm', 'initial')
      await page.keyboard.press('Tab')
      await addNode(page, testInfo, 'tool', 'initial', 'Enter')
      await page.keyboard.press('Tab')
      await addNode(page, testInfo, 'branch', 'initial', 'Space')
      await page.keyboard.press('Tab')
      const approval = await addNode(page, testInfo, 'approval', 'initial', 'Enter')
      await editSelectedTitle(page, approval)
      await screenshot(page, testInfo, 'four-types')

      // 添加仍保持用户的收起选择;展开后显示的必须是刚新增的节点。
      const toggle = page.getByTestId(TID.panelRightToggle)
      await toggle.click()
      await expect(toggle).toHaveAccessibleName('展开属性面板')
      await expect(toggle).toHaveAttribute('aria-pressed', 'false')
      const collapsedId = await addNode(page, testInfo, 'tool', 'collapsed', 'click', false)
      await expect(toggle).toHaveAccessibleName('展开属性面板')
      await expect(toggle).toHaveAttribute('aria-pressed', 'false')
      await screenshot(page, testInfo, 'add-while-collapsed')
      await toggle.click()
      await selectedPanel(page, collapsedId)
      await editSelectedTitle(page, collapsedId)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)

      // 另起空图,先让 branch 真实增高再添加,避免借拖动来规避占位风险。
      await page.getByTestId(TID.draftNew).click()
      await page.getByRole('button', { name: '丢弃并新建', exact: true }).click()
      await expect(page.getByText('空白画布', { exact: true })).toBeVisible()
      const branch = await addNode(page, testInfo, 'branch', 'tall')
      const branchCard = page.getByTestId(tid.nodeCard(branch))
      const originalHeight = (await branchCard.boundingBox())!.height
      const conditions = page.getByTestId(TID.nodePanel).locator('.panel-section').filter({
        has: page.getByRole('heading', { name: '分支条件', exact: true }),
      })
      for (let count = 1; count < 12; count++) {
        await conditions.getByRole('button', { name: '添加', exact: true }).click()
      }
      await expect(page.getByLabel('条件左值 12', { exact: true })).toBeVisible()
      await expect.poll(async () => (await branchCard.boundingBox())!.height).toBeGreaterThan(originalHeight + 80)
      const grownBranch = (await canvasDoc(page)).nodes.find((node) => node.id === branch)
      expect(grownBranch?.type).toBe('branch')
      if (grownBranch?.type !== 'branch') throw new Error('分支节点未呈现')
      expect(grownBranch.cases).toHaveLength(12)
      await addNode(page, testInfo, 'tool', 'tall')
      await addNode(page, testInfo, 'llm', 'tall')
      await addNode(page, testInfo, 'approval', 'tall')
      expect((await canvasDoc(page)).nodes.find((node) => node.id === branch)).toEqual(grownBranch)
      await screenshot(page, testInfo, 'tall-branch-and-three-types')
    })
  }
}
