// WorkflowsRoute 测试(T22.3 / F04 HIGH):发布成功反馈与编辑器卸载解耦。
// 发布成功会把工作区关掉(ws=null)+ 编辑器卸载,成功条必须留在路由层继续
// 可见并带「查看已发布」出口;open() 对非 DRAFT 的 definition 拒绝挂编辑器,
// 钉死「发布后深链回弹复活僵尸编辑器(顺手清掉成功条)」这条病根。
// fetch 全 stub 按 URL 路由(与 ExecutionsView.test 同法)。
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth.tsx'
import { loadBase } from '../model/testdata.ts'
import { session } from '../session.ts'
import { TID } from '../ui/testids.ts'
import { WorkflowProvider } from '../workflow/WorkflowProvider.tsx'
import { WorkflowsRoute } from './WorkflowsRoute.tsx'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const draftId = '7'
const draftDetail = {
  id: draftId,
  name: '告警分诊',
  version: 1,
  status: 'DRAFT',
  description: '',
  updated_at: '2026-10-15T00:00:00Z',
  dsl: loadBase('minimal-llm'),
}
const publishedDetail = { ...draftDetail, status: 'PUBLISHED', version: 2 }
const draftRow = {
  id: draftId,
  name: draftDetail.name,
  version: 1,
  status: 'DRAFT',
  description: '',
  updated_at: draftDetail.updated_at,
}

/** 与 AppLayout 同构:Provider 挂在 Routes 之上,路由切换不丢工作区状态。 */
function mount(initialEntry: string) {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <AuthProvider>
        <WorkflowProvider>
          <Routes>
            <Route
              path="/workflows"
              element={
                <>
                  <WorkflowsRoute />
                  <span data-testid="list-path-probe" />
                </>
              }
            />
            <Route path="/workflows/:draftId" element={<WorkflowsRoute />} />
            <Route path="/executions" element={<div data-testid="exec-probe" />} />
          </Routes>
        </WorkflowProvider>
      </AuthProvider>
    </MemoryRouter>,
  )
}

/** 发布前 GET 返回 DRAFT;publish POST 一旦被服务,详情与列表同步翻转
 * (镜像后端:草稿定格为 PUBLISHED,DRAFT 列表拿空)。 */
function stubFetch(onPublish?: () => void) {
  let published = false
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input).split('?')[0]
    if (path === `/api/v1/drafts/${draftId}/publish` && init?.method === 'POST') {
      published = true
      onPublish?.()
      return ok(publishedDetail)
    }
    if (path === `/api/v1/drafts/${draftId}`) return ok(published ? publishedDetail : draftDetail)
    if (path === '/api/v1/definitions') return ok({ items: published ? [] : [draftRow] })
    return ok({ error: 'not_found' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('WorkflowsRoute(发布成功反馈,F04)', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    session.clear()
  })

  it('点发布:编辑器卸载后成功条仍可见,带「查看已发布」出口;深链不复活编辑器', async () => {
    session.start('t', 'author')
    stubFetch()
    const user = userEvent.setup()
    mount(`/workflows/${draftId}`)

    await screen.findByTestId(TID.editor)
    expect(screen.queryByTestId(TID.workflowFlash)).not.toBeInTheDocument()

    await user.click(screen.getByTestId(TID.editorPublish))

    // 编辑器卸载(工作区关闭是发布语义),反馈不跟着走:成功条在路由层,
    // 左列 DRAFT 条目消失也不再有「工作流凭空消失」的观感。
    await waitFor(() => expect(screen.queryByTestId(TID.editor)).not.toBeInTheDocument())
    expect(screen.getByTestId(TID.workflowFlash)).toHaveTextContent('已发布 告警分诊 v2')
    expect(screen.getByTestId(TID.viewPublished)).toBeInTheDocument()

    // 深链退回列表地址(地址清理落定后不再有编辑器复活窗口);成功条跨路由
    // 重挂载仍可见(Provider 在 Routes 之上)。守卫本体由下一个用例隔离钉死。
    await waitFor(() => expect(screen.getByTestId('list-path-probe')).toBeInTheDocument())
    expect(screen.queryByTestId(TID.editor)).not.toBeInTheDocument()
    expect(screen.getByTestId(TID.workflowFlash)).toHaveTextContent('已发布 告警分诊 v2')

    // 「查看已发布」出口:跳执行面触发页(已发布定义在那里面向触发)。
    await user.click(screen.getByTestId(TID.viewPublished))
    await screen.findByTestId('exec-probe')
  })

  it('open() 只编草稿:深链直指 PUBLISHED definition 不挂编辑器,也不循环重取', async () => {
    session.start('t', 'author')
    // 详情恒为 PUBLISHED(不发布、只深链):守卫要在这条冷路径上拒绝挂载。
    const fetchMock = stubFetch()
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input).split('?')[0]
      if (path === `/api/v1/drafts/${draftId}`) return ok(publishedDetail)
      if (path === '/api/v1/definitions') return ok({ items: [] })
      return ok({ error: 'not_found' }, 404)
    })
    mount(`/workflows/${draftId}`)

    // 拒绝挂载:列表空态在场,编辑器与成功条都不出现(冷深链无 flash)。
    await waitFor(() => expect(screen.getByTestId(TID.draftEmpty)).toBeInTheDocument())
    expect(screen.queryByTestId(TID.editor)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.workflowFlash)).not.toBeInTheDocument()
    // 拒绝不改状态 → effect 依赖不变 → 不循环重取(恰好一次)。
    await Promise.resolve()
    await Promise.resolve()
    expect(fetchMock.mock.calls.filter(([u]) => String(u) === `/api/v1/drafts/${draftId}`)).toHaveLength(1)
  })

  it('发布失败:编辑器不卸载,走 issues 面,成功条不出现', async () => {
    session.start('t', 'author')
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const path = String(input).split('?')[0]
        if (path === `/api/v1/drafts/${draftId}/publish` && init?.method === 'POST') {
          return ok({ error: 'invalid_dsl', issues: [{ code: 'invalid_dsl', detail: 'llm.prompt 不能为空' }] }, 409)
        }
        if (path === `/api/v1/drafts/${draftId}`) return ok(draftDetail)
        if (path === '/api/v1/definitions') return ok({ items: [draftRow] })
        return ok({ error: 'not_found' }, 404)
      }),
    )
    const user = userEvent.setup()
    mount(`/workflows/${draftId}`)

    await screen.findByTestId(TID.editor)
    await user.click(screen.getByTestId(TID.editorPublish))

    // 失败路径:编辑器留在场(可改可重试),反馈走 IssuesPanel。
    await waitFor(() => expect(screen.getByTestId(TID.issues)).toBeInTheDocument())
    expect(screen.getByTestId(TID.editor)).toBeInTheDocument()
    expect(screen.queryByTestId(TID.workflowFlash)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.viewPublished)).not.toBeInTheDocument()
    // 深链不清理:编辑器还开着,地址仍指这条草稿。
    expect(screen.queryByTestId('list-path-probe')).not.toBeInTheDocument()
  })
})
