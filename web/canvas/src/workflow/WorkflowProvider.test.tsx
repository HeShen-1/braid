// F05:校验错误横幅的时序契约——保存失败即出现,编辑文档第一时间清除,
// 不再常驻到下次保存。fetch 全 stub 按 URL 路由(ExecutionsView 同款)。
import { render, screen, waitFor } from '@testing-library/react'
import { useEffect } from 'react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { tokenStore } from '../api.ts'
import { blankDoc } from '../defaults.ts'
import { IssuesPanel } from '../ui/IssuesPanel.tsx'
import { TID } from '../ui/testids.ts'
import { AuthProvider } from '../auth.tsx'
import { WorkflowProvider, useWorkflow } from './WorkflowProvider.tsx'

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubRoute(responder: (url: string, init?: RequestInit) => Response) {
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
