import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createMemoryRouter, Outlet, RouterProvider, useParams } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, RequireAuth } from '../auth.tsx'
import { LoginView } from '../LoginView.tsx'
import { session } from '../session.ts'
import { TID } from '../ui/testids.ts'
import { ApprovalLinkRedirect } from './ApprovalLinkRedirect.tsx'

function ApprovalTarget() {
  const { executionId, nodeId } = useParams()
  return <output>{executionId}/{nodeId}</output>
}

function mount(initial: string) {
  const router = createMemoryRouter([
    { path: '/login', element: <LoginView /> },
    {
      element: <RequireAuth><Outlet /></RequireAuth>,
      children: [
        { path: '/executions/:executionId/approvals/:nodeId', element: <ApprovalLinkRedirect /> },
        { path: '/approvals/:executionId/:nodeId', element: <ApprovalTarget /> },
      ],
    },
  ], { initialEntries: [initial] })
  render(<AuthProvider><RouterProvider router={router} /></AuthProvider>)
  return router
}

afterEach(() => { session.clear(); vi.unstubAllGlobals() })

describe('审批通知链接', () => {
  it('已登录时跳到对应的审批详情并保留查询参数', async () => {
    session.start('token', 'alice', ['approver'])
    const router = mount('/executions/123/approvals/review?source=mail#details')
    expect(await screen.findByText('123/review')).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/approvals/123/review')
    expect(router.state.location.search).toBe('?source=mail')
    expect(router.state.location.hash).toBe('#details')
  })

  it('未登录先登录，再打开邮件指定的审批详情', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({
      token: 'token', username: 'alice', roles: ['approver'], expires_at: '2999-01-01T00:00:00Z',
    }), { status: 200, headers: { 'Content-Type': 'application/json' } })))
    const router = mount('/executions/456/approvals/final_review')
    expect(await screen.findByTestId(TID.loginUsername)).toBeInTheDocument()
    expect(screen.queryByText('456/final_review')).not.toBeInTheDocument()
    fireEvent.change(screen.getByTestId(TID.loginUsername), { target: { value: 'alice' } })
    fireEvent.change(screen.getByTestId(TID.loginPassword), { target: { value: 'test-password' } })
    fireEvent.click(screen.getByTestId(TID.loginSubmit))
    expect(await screen.findByText('456/final_review')).toBeInTheDocument()
    await waitFor(() => expect(router.state.location.pathname).toBe('/approvals/456/final_review'))
  })
})
