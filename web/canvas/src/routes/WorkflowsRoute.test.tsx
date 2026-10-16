// WorkflowsRoute 测试(T22.3 / F04 HIGH):发布成功反馈与编辑器卸载解耦。
// 发布成功会把工作区关掉(ws=null)+ 编辑器卸载,成功条必须留在路由层继续
// 可见并带「查看已发布」出口;open() 对非 DRAFT 的 definition 拒绝挂编辑器,
// 钉死「发布后深链回弹复活僵尸编辑器(顺手清掉成功条)」这条病根。
// fetch 全 stub 按 URL 路由(与 ExecutionsView.test 同法)。
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, Link, Outlet, RouterProvider } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth.tsx'
import { loadBase } from '../model/testdata.ts'
import { session } from '../session.ts'
import { TID, tid } from '../ui/testids.ts'
import { WorkflowProvider } from '../workflow/WorkflowProvider.tsx'
import { WorkflowLeaveGuard } from '../workflow/WorkflowLeaveGuard.tsx'
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
function mount(initialEntry: string | string[]) {
  const router = createMemoryRouter([{
    element: (
      <AuthProvider><WorkflowProvider>
        <Link to="/executions">离开画布</Link>
        <WorkflowLeaveGuard />
        <Outlet />
      </WorkflowProvider></AuthProvider>
    ),
    children: [
      { path: '/workflows', element: <><WorkflowsRoute /><span data-testid="list-path-probe" /></> },
      { path: '/workflows/:draftId', element: <WorkflowsRoute /> },
      { path: '/executions', element: <div data-testid="exec-probe" /> },
    ],
  }], { initialEntries: typeof initialEntry === 'string' ? [initialEntry] : initialEntry })
  return { ...render(<RouterProvider router={router} />), router }
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

  it.each([{ roles: ['approver'] }, { roles: [] }])('角色 $roles 深链草稿也不能挂编辑器或发写请求', async ({ roles }) => {
    session.start('t', 'same-user', roles)
    const fetcher = stubFetch()
    mount(`/workflows/${draftId}`)
    expect(await screen.findByRole('heading', { name: '已发布与版本历史' })).toBeInTheDocument()
    expect(screen.queryByTestId(TID.editor)).not.toBeInTheDocument()
    expect(screen.queryByTestId(TID.draftNew)).not.toBeInTheDocument()
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'POST' || init?.method === 'PUT')).toBe(false)
    expect(fetcher.mock.calls.some(([url]) => String(url) === `/api/v1/drafts/${draftId}`)).toBe(false)
  })

  it('点发布:编辑器卸载后成功条仍可见,带「查看已发布」出口;深链不复活编辑器', async () => {
    session.start('t', 'author', ['author'])
    stubFetch()
    const user = userEvent.setup()
    const { router } = mount(`/workflows/${draftId}`)

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

    // 「查看已发布」进入工作流版本浏览，URL 可刷新恢复该视图。
    await user.click(screen.getByTestId(TID.viewPublished))
    expect(await screen.findByRole('heading', { name: '已发布与版本历史' })).toBeInTheDocument()
    expect(router.state.location.search).toBe('?view=versions')
  })

  it('open() 只编草稿:深链直指 PUBLISHED definition 不挂编辑器,也不循环重取', async () => {
    session.start('t', 'author', ['author'])
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
    session.start('t', 'author', ['author'])
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

  it('新建前保护未保存内容;取消回入口焦点,确认才丢弃', async () => {
    session.start('t', 'author', ['author'])
    stubFetch()
    const user = userEvent.setup()
    mount(`/workflows/${draftId}`)
    await screen.findByTestId(TID.editor)
    await user.click(screen.getByRole('button', { name: '工作流设置' }))
    await user.type(screen.getByLabelText('描述'), '未保存描述')
    await user.click(screen.getByTestId(TID.draftNew))
    expect(screen.getByRole('group', { name: '新建前确认' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.getByTestId(TID.draftNew)).toHaveFocus()
    expect(screen.getByTestId(TID.editorJson)).toHaveTextContent('未保存描述')

    await user.click(screen.getByTestId(TID.draftNew))
    await user.click(screen.getByRole('button', { name: '丢弃并新建' }))
    await waitFor(() => expect(screen.getByTestId(TID.editorJson)).toHaveTextContent('untitled-workflow'))
    expect(screen.getByTestId(TID.editorJson)).not.toHaveTextContent('未保存描述')
  })

  it('直接切换草稿 URL/浏览器后退均保护;确认后停在目标草稿,不被旧状态拉回', async () => {
    session.start('t', 'author', ['author'])
    const second = { ...draftDetail, id: '8', name: 'second', dsl: { ...draftDetail.dsl, name: 'second' } }
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input).split('?')[0]
      if (path === '/api/v1/definitions') return ok({ items: [draftRow, { ...draftRow, id: '8', name: 'second' }] })
      return ok(path.endsWith('/8') ? second : draftDetail)
    }))
    const user = userEvent.setup()
    const { router } = mount(['/workflows/8', `/workflows/${draftId}`])
    await screen.findByTestId(TID.editor)
    await user.click(screen.getByRole('button', { name: '工作流设置' }))
    await user.type(screen.getByLabelText('描述'), '保留我')

    await user.click(screen.getByTestId(tid.draftItem('8')))
    await user.click(screen.getByRole('button', { name: '放弃' }))
    expect(router.state.location.pathname).toBe(`/workflows/${draftId}`)
    expect(screen.getByTestId(tid.draftItem('8'))).toHaveFocus()
    expect(screen.getByTestId(TID.editorJson)).toHaveTextContent('保留我')

    await act(async () => { await router.navigate(-1) })
    expect(screen.getByRole('group', { name: '未保存的画布' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '继续离开' }))
    await waitFor(() => expect(screen.getByTestId(TID.editorJson)).toHaveTextContent('second'))
    expect(router.state.location.pathname).toBe('/workflows/8')
    expect(screen.getByTestId(TID.editorJson)).not.toHaveTextContent('保留我')
  })

  it('离开主导航与刷新有脏态保护;保存成功后解除,单纯选择节点不误报', async () => {
    session.start('t', 'author', ['author'])
    stubFetch()
    const user = userEvent.setup()
    mount(`/workflows/${draftId}`)
    await screen.findByTestId(TID.editor)
    fireEvent.click(screen.getByTestId(tid.nodeCard('only')))
    let unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(false)

    await user.click(screen.getByRole('button', { name: '工作流设置' }))
    await user.type(screen.getByLabelText('描述'), '编辑内容')
    unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(true)
    await user.click(screen.getByRole('link', { name: '离开画布' }))
    expect(screen.queryByTestId('exec-probe')).not.toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('link', { name: '离开画布' })).toHaveFocus()

    await user.click(screen.getByTestId(TID.editorSave))
    await screen.findByText('已保存')
    unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(false)
    await user.click(screen.getByRole('link', { name: '离开画布' }))
    await screen.findByTestId('exec-probe')
  })

  it('B 详情在飞时后退 A 会废弃 B 的迟到响应,URL 与画布保持 A', async () => {
    session.start('t', 'author', ['author'])
    let resolve!: (response: Response) => void
    const pending = new Promise<Response>((r) => { resolve = r })
    let requested = false
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input).split('?')[0]
      if (path.endsWith('/8')) { requested = true; return pending }
      if (path === '/api/v1/definitions') return ok({ items: [draftRow, { ...draftRow, id: '8', name: 'second' }] })
      return ok(draftDetail)
    }))
    const user = userEvent.setup()
    const { router } = mount(`/workflows/${draftId}`)
    await screen.findByTestId(TID.editor)
    await user.click(screen.getByTestId(tid.draftItem('8')))
    await waitFor(() => expect(requested).toBe(true))
    await act(async () => { await router.navigate(-1) })
    await act(async () => {
      resolve(ok({ ...draftDetail, id: '8', dsl: { ...draftDetail.dsl, name: 'second' } }))
    })
    expect(router.state.location.pathname).toBe(`/workflows/${draftId}`)
    expect(screen.getByTestId(TID.editorJson)).toHaveTextContent('minimal-llm')
    expect(screen.getByTestId(TID.editorJson)).not.toHaveTextContent('second')
  })
})
