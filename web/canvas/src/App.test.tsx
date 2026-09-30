// App 集成流(fetch 全 stub):登录 → 草稿列表 → 开草稿进画布 →
// 保存(PUT 原文)/发布 → Issues 聚合回显 → 401 登出。
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from './App.tsx'
import { loadBase } from './model/testdata.ts'

const calls: { method: string; url: string; body?: string }[] = []

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubFetch(router: Router) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      // 不构造 Request(jsdom 下 undici 对相对 URL 的构造会抛错),
      // method/url/body 从参数直取;path 剥掉 jsdom origin 前缀。
      const rawUrl = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      const method = (
        init?.method ?? (typeof input !== 'string' && !(input instanceof URL) ? input.method : 'GET')
      ).toUpperCase()
      const path = rawUrl.replace(/^https?:\/\/[^/]+/, '')
      calls.push({ method, url: path, body: typeof init?.body === 'string' ? init.body : undefined })
      const res = router(method, path)
      if (res === undefined) throw new Error(`unstubbed ${method} ${path}`)
      return res
    }),
  )
}

type Router = (method: string, path: string) => Response | undefined

// Response body 单次消费:跨用例复用同一 Response 会让第二次 .json() 抛错,工厂化。
const loginOk = () => json(200, { token: 't1', token_type: 'Bearer', expires_at: '2026-10-01T00:00:00Z' })
const DEMO_DRAFT = {
  id: '7',
  name: 'demo-wf',
  version: null,
  status: 'DRAFT',
  description: '',
  updated_at: '2026-09-30T12:00:00Z',
}

function defaultRouter(): Router {
  return (method, path) => {
    if (method === 'POST' && path === '/api/v1/auth/login') return loginOk()
    if (method === 'GET' && path === '/api/v1/definitions?status=DRAFT') return json(200, { items: [DEMO_DRAFT] })
    if (method === 'GET' && path === '/api/v1/drafts/7') return json(200, { ...DEMO_DRAFT, dsl: loadBase('minimal-llm') })
    if (method === 'PUT' && path === '/api/v1/drafts/7') return json(200, DEMO_DRAFT)
    if (method === 'POST' && path === '/api/v1/drafts/7/publish')
      return json(200, { ...DEMO_DRAFT, version: 3, status: 'PUBLISHED' })
    return undefined
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  sessionStorage.clear()
  calls.length = 0
})

describe('App(登录 → 列表 → 画布 → 保存/发布)', () => {
  it('全链路:登录、开草稿、保存 PUT 原文、发布回显版本', async () => {
    const user = userEvent.setup()
    stubFetch(defaultRouter())
    render(<App />)

    await user.type(screen.getByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('密码'), 'secret')
    await user.click(screen.getByTestId('login-submit'))

    await user.click(await screen.findByTestId('draft-7'))
    // 画布渲染草稿节点(minimal-llm 唯一节点 only 的模板首行)。
    expect(await screen.findByText('说点什么')).toBeInTheDocument()

    await user.click(screen.getByTestId('save-draft'))
    await screen.findByText('已保存草稿 #7')
    const put = calls.find((c) => c.method === 'PUT' && c.url.endsWith('/drafts/7'))
    expect(put?.body).toContain('"dsl": "1.0"')
    expect(put?.body).toContain('"name": "minimal-llm"')
    // 位置随保存写入(MISS 时 autoLayout 补位后导出)。
    expect(put?.body).toContain('"position"')

    await user.click(screen.getByTestId('publish'))
    await screen.findByText('已发布 demo-wf v3')
  }, 20000)

  it('保存回显服务端 Issues(400 invalid_dsl 聚合)', async () => {
    const user = userEvent.setup()
    stubFetch((method, path) => {
      if (method === 'PUT' && path === '/api/v1/drafts/7')
        return json(400, { error: 'invalid_dsl', issues: [{ code: 'bad_ref', node: 'parse', ref: '{{nodes.x.output.y}}', detail: '未知引用目标' }] })
      return defaultRouter()(method, path)
    })
    render(<App />)
    await user.type(screen.getByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('密码'), 'secret')
    await user.click(screen.getByTestId('login-submit'))
    await user.click(await screen.findByTestId('draft-7'))
    await user.click(await screen.findByTestId('save-draft'))

    const panel = await screen.findByTestId('issues')
    expect(panel).toHaveTextContent('bad_ref')
    expect(panel).toHaveTextContent('parse')
    expect(panel).toHaveTextContent('未知引用目标')
  }, 20000)

  it('登录 401:防枚举文案', async () => {
    const user = userEvent.setup()
    stubFetch((method, path) => {
      if (method === 'POST' && path === '/api/v1/auth/login') return json(401, { error: 'invalid_credentials' })
      return undefined
    })
    render(<App />)
    await user.type(screen.getByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('密码'), 'wrong')
    await user.click(screen.getByTestId('login-submit'))
    expect(await screen.findByRole('alert')).toHaveTextContent('登录失败')
  })

  it('401 会话失效:任意 API 401 → 清 token 回登录', async () => {
    const user = userEvent.setup()
    stubFetch((method, path) => {
      if (method === 'POST' && path === '/api/v1/auth/login') return loginOk()
      if (method === 'GET' && path === '/api/v1/definitions?status=DRAFT') return json(401, { error: 'token_expired' })
      return undefined
    })
    render(<App />)
    await user.type(screen.getByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('密码'), 'secret')
    await user.click(screen.getByTestId('login-submit'))
    await waitFor(() => expect(screen.getByLabelText('用户名')).toBeInTheDocument())
    expect(sessionStorage.getItem('braid_canvas_token')).toBeNull()
  })

  it('网络异常(非 ApiError):保存反馈 network_error,不停留在过期 flash(对抗审查回归)', async () => {
    const user = userEvent.setup()
    stubFetch((method, path) => {
      if (method === 'POST' && path === '/api/v1/auth/login') return loginOk()
      if (method === 'GET' && path === '/api/v1/definitions?status=DRAFT') return json(200, { items: [DEMO_DRAFT] })
      if (method === 'GET' && path === '/api/v1/drafts/7') return json(200, { ...DEMO_DRAFT, dsl: loadBase('minimal-llm') })
      throw new TypeError('fetch failed') // 模拟断网(async 桩内抛 = fetch reject)
    })
    render(<App />)
    await user.type(screen.getByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('密码'), 'secret')
    await user.click(screen.getByTestId('login-submit'))
    await user.click(await screen.findByTestId('draft-7'))
    await user.click(await screen.findByTestId('save-draft'))
    const panel = await screen.findByTestId('issues')
    expect(panel).toHaveTextContent('network_error')
    expect(panel).toHaveTextContent('网络异常')
  }, 20000)
})
