// UI 偏好载体(决策 6)的容错锚:localStorage 不可用时要退默认值,
// 而不是把渲染炸掉——偏好丢了不算故障。
import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { PANEL_LEFT_KEY, useCollapsed } from './prefs.ts'

beforeEach(() => {
  localStorage.clear()
})

describe('useCollapsed', () => {
  it('缺省展开;写 true 落盘为 1,写 false 落盘为 0', () => {
    const { result } = renderHook(() => useCollapsed(PANEL_LEFT_KEY))
    expect(result.current[0]).toBe(false)
    act(() => result.current[1](true))
    expect(result.current[0]).toBe(true)
    expect(localStorage.getItem(PANEL_LEFT_KEY)).toBe('1')
    act(() => result.current[1](false))
    expect(localStorage.getItem(PANEL_LEFT_KEY)).toBe('0')
  })

  it('读回既有偏好;非法值退默认(展开)', () => {
    localStorage.setItem(PANEL_LEFT_KEY, '1')
    expect(renderHook(() => useCollapsed(PANEL_LEFT_KEY)).result.current[0]).toBe(true)
    localStorage.setItem(PANEL_LEFT_KEY, 'yes')
    expect(renderHook(() => useCollapsed(PANEL_LEFT_KEY)).result.current[0]).toBe(false)
  })

  it('localStorage 被封:读退默认、写不抛', () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const { result } = renderHook(() => useCollapsed(PANEL_LEFT_KEY))
    expect(result.current[0]).toBe(false)
    act(() => result.current[1](true))
    expect(result.current[0]).toBe(true)
    get.mockRestore()
    set.mockRestore()
  })
})
