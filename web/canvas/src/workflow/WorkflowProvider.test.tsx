// F05:校验错误横幅的时序契约——保存失败即出现,编辑文档第一时间清除,
// 不再常驻到下次保存。fetch 全 stub 按 URL 路由(ExecutionsView 同款)。
import { act, render, renderHook, screen, waitFor } from '@testing-library/react'
import { useEffect } from 'react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { tokenStore } from '../api.ts'
import { blankDoc } from '../defaults.ts'
import { loadBase } from '../model/testdata.ts'
import { serializeDoc } from '../model/convert.ts'
import type { WorkflowDoc } from '../model/dsl.ts'
import { IssuesPanel } from '../ui/IssuesPanel.tsx'
import { TID } from '../ui/testids.ts'
import { AuthProvider } from '../auth.tsx'
import { WorkflowProvider, useWorkflow } from './WorkflowProvider.tsx'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function deferredResponse() {
  let resolve!: (response: Response) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<Response>((r, j) => { resolve = r; reject = j })
  return { promise, resolve, reject }
}

function stubRoute(responder: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => responder(String(input), init))
  vi.stubGlobal('fetch', fn)
  return fn
}

function stubSaveRejected() {
  return stubRoute((url, init) => {
    if (init?.method === 'POST' && url === '/api/v1/definitions') {
      return ok(
        {
          error: 'invalid_dsl',
          issues: [{ code: 'invalid_document', ref: '/nodes/0/prompt/template', detail: 'minLength: got 0, want 1' }],
        },
        400,
      )
    }
    return ok({ items: [] })
  })
}

// 测试挂点:暴露 provider 的 issues 渲染与两个动作(保存 / 触发一次编辑)。
// 挂载即 newLocal 对齐真实旅程(新建本地草稿 → 保存失败 → 编辑)。
function Harness() {
  const { issues, save, onDocChange, newLocal } = useWorkflow()
  useEffect(() => {
    newLocal()
  }, [newLocal])
  return (
    <div>
      <IssuesPanel issues={issues} />
      <button onClick={() => void save(blankDoc())}>保存草稿</button>
      <button onClick={() => onDocChange(blankDoc())}>编辑</button>
    </div>
  )
}

function mount() {
  return render(
    <AuthProvider>
      <WorkflowProvider>
        <Harness />
      </WorkflowProvider>
    </AuthProvider>,
  )
}

beforeEach(() => {
  sessionStorage.clear()
  tokenStore.set('test-token')
  sessionStorage.setItem('braid_canvas_user', 'tester')
})

describe('WorkflowProvider(保存当前画布后发布,T23.5)', () => {
  const doc = loadBase('minimal-llm')
  const edited = { ...doc, description: '当前画布内容' }
  const detail = { id: '7', name: doc.name, status: 'DRAFT', version: null, dsl: doc }

  function mountWorkspace() {
    return renderHook(useWorkflow, {
      wrapper: ({ children }) => <AuthProvider><WorkflowProvider>{children}</WorkflowProvider></AuthProvider>,
    })
  }

  it('列表首读失败保留错误，重试失败不清上次列表，成功后清错误', async () => {
    const first = deferredResponse()
    let round = 0
    const row = { id: '7', name: 'existing', status: 'DRAFT', version: null }
    stubRoute(() => {
      round++
      if (round === 1) return first.promise
      if (round === 3) return Promise.reject(new TypeError('offline'))
      return ok({ items: [row] })
    })
    const { result } = mountWorkspace()
    expect(result.current.loading).toBe(true)
    await act(async () => { first.reject(new TypeError('offline')) })
    expect(result.current.loading).toBe(false)
    expect(result.current.listError).toContain('检查网络')
    await act(async () => { await result.current.refresh() })
    expect(result.current.drafts).toEqual([row])
    await act(async () => { await result.current.refresh() })
    expect(result.current.drafts).toEqual([row])
    expect(result.current.listError).toContain('检查网络')
    await act(async () => { await result.current.refresh() })
    expect(result.current.listError).toBe('')
  })

  it('迟到的列表读取失败不覆盖已完成的新请求', async () => {
    const old = deferredResponse()
    let round = 0
    stubRoute(() => ++round === 1 ? old.promise : ok({ items: [{ id: 'new' }] }))
    const { result } = mountWorkspace()
    await act(async () => { await result.current.refresh() })
    await act(async () => { old.reject(new TypeError('late offline')) })
    expect(result.current.drafts).toEqual([{ id: 'new' }])
    expect(result.current.listError).toBe('')
    expect(result.current.loading).toBe(false)
  })

  it('直接发布先 PUT 当前画布,然后 POST publish;成功才关闭工作区', async () => {
    const writes: string[] = []
    let saved = doc
    stubRoute((url, init) => {
      if (init?.method === 'PUT') {
        writes.push('save')
        saved = JSON.parse(String(init.body))
        return ok({ ...detail, dsl: saved })
      }
      if (url.endsWith('/publish')) {
        writes.push('publish')
        expect(saved).toEqual(JSON.parse(serializeDoc(edited)))
        return ok({ ...detail, status: 'PUBLISHED', version: 1, dsl: saved })
      }
      return ok(url.includes('/drafts/') ? detail : { items: [] })
    })
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    act(() => result.current.onDocChange(edited))
    await act(async () => { expect(await result.current.publish(edited)).toBe(true) })
    expect(writes).toEqual(['save', 'publish'])
    expect(result.current.ws).toBeNull()
    expect(result.current.flash).toContain('已发布')
  })

  it('保存失败保持当前内容与脏态,零 publish 请求', async () => {
    const fetchMock = stubRoute((url, init) => {
      if (init?.method === 'PUT') return ok({ error: 'invalid_dsl', issues: [{ code: 'invalid_dsl', detail: '保存失败' }] }, 400)
      return ok(url.includes('/drafts/') ? detail : { items: [] })
    })
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    act(() => result.current.onDocChange(edited))
    await act(async () => { expect(await result.current.publish(edited)).toBe(false) })
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/publish'))).toHaveLength(0)
    expect(result.current.ws?.doc).toEqual(edited)
    expect(result.current.dirty).toBe(true)
    expect(result.current.issues[0].detail).toBe('保存失败')
  })

  it('保存期间继续编辑:迟到保存只更新保存基线,不覆盖新内容,也不发布旧快照', async () => {
    const pending = deferredResponse()
    const fetchMock = stubRoute((url, init) => {
      if (init?.method === 'PUT') return pending.promise
      return ok(url.includes('/drafts/') ? detail : { items: [] })
    })
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    act(() => result.current.onDocChange(edited))
    let publishing: Promise<boolean>
    act(() => { publishing = result.current.publish(edited) })
    const later = { ...edited, description: '请求发出后的修改' }
    act(() => result.current.onDocChange(later))
    await act(async () => {
      pending.resolve(ok({ ...detail, dsl: edited }))
      expect(await publishing!).toBe(false)
    })
    expect(result.current.ws?.doc).toEqual(later)
    expect(result.current.dirty).toBe(true)
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/publish'))).toHaveLength(0)
  })

  it('旧工作区的保存响应不能覆盖后来新建的画布', async () => {
    const pending = deferredResponse()
    stubRoute((url, init) => init?.method === 'PUT' ? pending.promise : ok(url.includes('/drafts/') ? detail : { items: [] }))
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    let saving: Promise<void>
    act(() => { saving = result.current.save(edited) })
    act(() => result.current.newLocal())
    await act(async () => {
      pending.resolve(ok({ ...detail, dsl: edited }))
      await saving!
    })
    expect(result.current.ws?.draftId).toBeNull()
    expect(result.current.ws?.doc).toEqual(blankDoc())
    expect(result.current.flash).toBe('')
  })

  it.each([200, 409, 'network'] as const)('publish 请求已发后继续编辑:状态 %s 均保留后续内容与脏态', async (status) => {
    const pending = deferredResponse()
    let started = false
    stubRoute((url) => {
      if (url.endsWith('/publish')) { started = true; return pending.promise }
      return ok(url.includes('/drafts/') ? detail : { items: [] })
    })
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    let publishing: Promise<boolean>
    act(() => { publishing = result.current.publish(edited) })
    await waitFor(() => expect(started).toBe(true))
    const later = { ...edited, description: '发布请求后的修改' }
    act(() => result.current.onDocChange(later))
    await act(async () => {
      if (status === 'network') pending.reject(new Error('network down'))
      else pending.resolve(ok(status === 200 ? { ...detail, status: 'PUBLISHED', version: 1 } : { error: 'conflict' }, status))
      expect(await publishing!).toBe(false)
    })
    expect(result.current.ws?.doc).toEqual(later)
    expect(result.current.dirty).toBe(true)
    expect(result.current.ws?.draftId).toBe(status === 200 ? null : '7')
    if (status === 200) expect(result.current.flash).toContain('后续更改尚未保存')
    else expect(result.current.issues[0].code).toBe(status === 'network' ? 'network_error' : 'conflict')
  })

  it.each(['change', 'remove', 'unchanged'] as const)('迟到发布校验错误:节点 %s 后不回挂旧字段错误,未改文档仍展示', async (edit) => {
    const toolDoc: WorkflowDoc = {
      ...doc,
      nodes: [{ id: 'only', type: 'tool', tool: 'unregistered_tool', inputs: { path: '/data/a.txt' }, outputs: {} }],
    }
    const pending = deferredResponse()
    let started = false
    stubRoute((url) => {
      if (url.endsWith('/publish')) { started = true; return pending.promise }
      return ok(url.includes('/drafts/') ? { ...detail, dsl: toolDoc } : { items: [] })
    })
    const { result } = mountWorkspace()
    await act(async () => { await result.current.open('7') })
    let publishing: Promise<boolean>
    act(() => { publishing = result.current.publish(toolDoc) })
    await waitFor(() => expect(started).toBe(true))
    const current: WorkflowDoc = edit === 'remove'
      ? { ...toolDoc, nodes: [] }
      : edit === 'change'
        ? { ...toolDoc, nodes: [{ ...toolDoc.nodes[0], type: 'tool', tool: 'file_read', inputs: { path: '/data/a.txt' } }] }
        : toolDoc
    if (edit !== 'unchanged') act(() => result.current.onDocChange(current))
    const issue = { code: 'tool_not_allowed', ref: '/nodes/0/tool', detail: '旧节点工具 unregistered_tool 未获授权' }
    await act(async () => {
      pending.resolve(ok({ error: 'invalid_dsl', issues: [issue] }, 400))
      expect(await publishing!).toBe(false)
    })
    expect(result.current.ws?.doc).toEqual(current)
    expect(result.current.dirty).toBe(edit !== 'unchanged')
    if (edit === 'unchanged') expect(result.current.issues).toEqual([issue])
    else {
      expect(result.current.issues).toEqual([])
      expect(result.current.flash).toContain('先前内容发布失败')
      expect(result.current.flash).toContain('再次发布')
    }
  })

  it('迟到 open 在新建后失效;后发的打开请求优先', async () => {
    const first = deferredResponse()
    stubRoute((url) => url.endsWith('/7') ? first.promise : ok(url.includes('/drafts/') ? { ...detail, id: '8' } : { items: [] }))
    const { result } = mountWorkspace()
    let opening: Promise<boolean>
    act(() => { opening = result.current.open('7') })
    act(() => result.current.newLocal())
    await act(async () => { await result.current.open('8') })
    await act(async () => {
      first.resolve(ok(detail))
      expect(await opening!).toBe(false)
    })
    expect(result.current.ws?.draftId).toBe('8')
  })

  it('同一工作区的并发保存只发一个 create;create 迟到不能锁住后来改过的名称', async () => {
    const pending = deferredResponse()
    const fetchMock = stubRoute((_url, init) => init?.method === 'POST' ? pending.promise : ok({ items: [] }))
    const { result } = mountWorkspace()
    act(() => result.current.newLocal())
    let saving: Promise<void>
    act(() => { saving = result.current.save(doc) })
    await act(async () => { await result.current.save(doc) })
    const renamed = { ...doc, name: 'renamed' }
    act(() => result.current.onDocChange(renamed))
    await act(async () => {
      pending.resolve(ok(detail))
      await saving!
    })
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
    expect(result.current.ws?.draftId).toBeNull()
    expect(result.current.ws?.doc.name).toBe('renamed')
    expect(result.current.dirty).toBe(true)
  })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('WorkflowProvider(校验错误时序,F05)', () => {
  it('保存失败:结构化 issues 出现;第一次编辑即清除,不再常驻', async () => {
    const user = userEvent.setup()
    stubSaveRejected()
    mount()

    await user.click(screen.getByRole('button', { name: '保存草稿' }))
    const panel = await screen.findByTestId(TID.issues)
    expect(panel).toHaveTextContent('minLength')

    await user.click(screen.getByRole('button', { name: '编辑' }))
    await waitFor(() => expect(screen.queryByTestId(TID.issues)).not.toBeInTheDocument())
  })

  it('无错误时编辑不产生横幅(清除逻辑不空造)', async () => {
    const user = userEvent.setup()
    stubSaveRejected()
    mount()

    await user.click(screen.getByRole('button', { name: '编辑' }))
    expect(screen.queryByTestId(TID.issues)).not.toBeInTheDocument()
  })
})
