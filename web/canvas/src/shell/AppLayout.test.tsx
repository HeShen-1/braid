import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, RequireAuth } from '../auth.tsx'
import { blankDoc } from '../defaults.ts'
import { session } from '../session.ts'
import { TID } from '../ui/testids.ts'
import { useWorkflow } from '../workflow/WorkflowProvider.tsx'
import { AppLayout } from './AppLayout.tsx'

function Harness() {
  const { newLocal, save, ws } = useWorkflow()
  return <>
    <button onClick={newLocal}>新建画布</button>
    <button onClick={() => void save(blankDoc())}>保存草稿</button>
    {ws !== null ? <span>编辑内容仍在</span> : null}
  </>
}

function mount() {
  session.start('t', 'author')
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => new Response(
    JSON.stringify(init?.method === 'POST' ? { error: 'unauthorized' } : { items: [], page: { total: 0 } }),
    { status: init?.method === 'POST' ? 401 : 200, headers: { 'Content-Type': 'application/json' } },
  )))
  const router = createMemoryRouter([
    { element: <RequireAuth><AppLayout /></RequireAuth>, children: [{ path: '/workflows', element: <Harness /> }] },
    { path: '/login', element: <span>登录页</span> },
  ], { initialEntries: ['/workflows'] })
  render(<AuthProvider><RouterProvider router={router} /></AuthProvider>)
}

afterEach(() => { vi.unstubAllGlobals(); session.clear() })

describe('AppLayout(未保存退出保护,T23.5)', () => {
  it('主动退出先确认;取消保留编辑和入口焦点,确认才退出', async () => {
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByRole('button', { name: '新建画布' }))
    await user.click(screen.getByTestId(TID.logout))
    expect(screen.getByRole('group', { name: '退出前确认' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.getByTestId(TID.logout)).toHaveFocus()
    expect(screen.getByText('编辑内容仍在')).toBeInTheDocument()
    expect(session.authed).toBe(true)
    await user.click(screen.getByTestId(TID.logout))
    await user.click(screen.getByRole('button', { name: '仍要退出' }))
    await screen.findByText('登录页')
    expect(session.authed).toBe(false)
  })

  it('401 强制失效仍立即退出,不被未保存确认阻挡', async () => {
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByRole('button', { name: '新建画布' }))
    await user.click(screen.getByRole('button', { name: '保存草稿' }))
    await screen.findByText('登录页')
    expect(screen.queryByRole('group', { name: '退出前确认' })).not.toBeInTheDocument()
    expect(session.authed).toBe(false)
  })
})
