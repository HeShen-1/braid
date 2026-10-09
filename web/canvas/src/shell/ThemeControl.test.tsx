import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { THEME_KEY } from '../theme.ts'
import { tid } from '../ui/testids.ts'
import { ThemeControl } from './ThemeControl.tsx'

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset.theme
})

describe('ThemeControl(三态)', () => {
  it('三颗按钮,默认高亮深色,并把 data-theme 落定', () => {
    render(<ThemeControl />)
    expect(screen.getByTestId(tid.themeOption('dark'))).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByTestId(tid.themeOption('light'))).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByTestId(tid.themeOption('auto'))).toHaveAttribute('aria-pressed', 'false')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('点浅色:data-theme 与 localStorage 同时变', async () => {
    const user = userEvent.setup()
    render(<ThemeControl />)
    await user.click(screen.getByTestId(tid.themeOption('light')))
    expect(document.documentElement.dataset.theme).toBe('light')
    expect(localStorage.getItem(THEME_KEY)).toBe('light')
    expect(screen.getByTestId(tid.themeOption('light'))).toHaveAttribute('aria-pressed', 'true')
  })

  it('跨会话:上次选 auto,重挂载即高亮 auto 并落定属性', () => {
    localStorage.setItem(THEME_KEY, 'auto')
    render(<ThemeControl />)
    expect(screen.getByTestId(tid.themeOption('auto'))).toHaveAttribute('aria-pressed', 'true')
    expect(document.documentElement.dataset.theme).toBe('auto')
  })

  it('可及名:每颗按钮都有中文名(仅图标时靠 sr-only)', () => {
    render(<ThemeControl />)
    expect(screen.getByRole('button', { name: '跟随系统' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '浅色' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '深色' })).toBeInTheDocument()
    expect(screen.getByRole('group', { name: '主题' })).toBeInTheDocument()
  })
})
