import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, useAuth } from './auth.tsx'
import { session } from './session.ts'
import { tokenStore } from './api.ts'

function Probe() {
  const auth = useAuth()
  return <><output>{JSON.stringify({ user: auth.user, roles: auth.roles, expiresAt: auth.expiresAt, author: auth.canAuthor, approve: auth.canApprove })}</output><button onClick={() => void auth.login('typed-name', 'pw')}>登录</button></>
}

afterEach(() => { session.clear(); vi.unstubAllGlobals(); vi.useRealTimers() })

describe('登录身份与真实能力', () => {
  it('消费服务端规范用户名/角色/到期字段，刷新后仍保留；账号名不推导角色', async () => {
    const expires = '2999-01-01T00:00:00Z'
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ token: 't', token_type: 'Bearer', username: 'canonical-approver', roles: ['approver'], expires_at: expires }))))
    const first = render(<AuthProvider><Probe /></AuthProvider>)
    await act(async () => { fireEvent.click(screen.getByText('登录')) })
    expect(screen.getByRole('status')).toHaveTextContent('"user":"canonical-approver"')
    expect(screen.getByRole('status')).toHaveTextContent('"author":false,"approve":true')
    expect(session.user).toBe('canonical-approver')
    expect(session.roles).toEqual(['approver'])
    expect(session.expiresAt).toBe(expires)
    first.unmount()
    render(<AuthProvider><Probe /></AuthProvider>)
    expect(screen.getByRole('status')).toHaveTextContent('"user":"canonical-approver"')
    expect(screen.getByRole('status')).toHaveTextContent('"roles":["approver"]')
  })

  it.each([
    [['author'], true, false], [['approver'], false, true], [['admin'], true, true], [['author', 'approver'], true, true], [[], false, false],
  ] as const)('按真实角色 %j 暴露能力', (roles, author, approve) => {
    session.start('t', 'same-user', [...roles])
    render(<AuthProvider><Probe /></AuthProvider>)
    expect(screen.getByRole('status')).toHaveTextContent(`"author":${author},"approve":${approve}`)
  })

  it('旧会话与损坏元数据不猜JWT/账号名的权限', () => {
    tokenStore.set('not-a-jwt')
    sessionStorage.setItem('braid_canvas_user', 'admin')
    sessionStorage.setItem('braid_canvas_roles', 'invalid-json')
    render(<AuthProvider><Probe /></AuthProvider>)
    expect(screen.getByRole('status')).toHaveTextContent('"roles":[]')
    expect(screen.getByRole('status')).toHaveTextContent('"author":false,"approve":false')
  })

  it('消费 expires_at 到期清除凭证和身份', async () => {
    vi.useFakeTimers()
    session.start('t', 'author', ['author'], new Date(Date.now() + 1000).toISOString())
    render(<AuthProvider><Probe /></AuthProvider>)
    await act(async () => { await vi.advanceTimersByTimeAsync(1001) })
    expect(screen.getByRole('status')).toHaveTextContent('"user":null')
    expect(tokenStore.get()).toBeNull()
    expect(session.roles).toEqual([])
  })
})
