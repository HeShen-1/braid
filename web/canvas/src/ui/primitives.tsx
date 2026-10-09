// 组件原语：世界只借出四样东西（字体、调色板、密度、一个签名动作），
// 布局与控件一律是 web 平台的标准件。这里就是那套标准件。
import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { I } from './I.tsx'
import { cx } from './cx.ts'
import { nodeStatus, execStatus } from './status.ts'
import type { LucideIcon } from 'lucide-react'

/* ---------------- 按钮 ---------------- */

type ButtonVariant = 'primary' | 'default' | 'quiet' | 'danger'

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: 'md' | 'sm'
  icon?: LucideIcon
  /** 仅图标：必须给 aria-label。 */
  iconOnly?: boolean
}

export function Button({
  variant = 'default',
  size = 'md',
  icon,
  iconOnly = false,
  className,
  children,
  type = 'button',
  ...rest
}: ButtonProps) {
  const variantClass =
    variant === 'primary' ? 'btn-primary' : variant === 'quiet' ? 'btn-quiet' : variant === 'danger' ? 'btn-danger' : ''
  return (
    <button
      type={type}
      className={cx('btn', variantClass, size === 'sm' && 'btn-sm', iconOnly && 'btn-icon', className)}
      {...rest}
    >
      {icon !== undefined ? <I glyph={icon} size={size === 'sm' ? 12 : 14} /> : null}
      {iconOnly ? null : children}
    </button>
  )
}

/* ---------------- 面板 ---------------- */

export function Panel({ className, children }: { className?: string; children: ReactNode }) {
  return <section className={cx('panel', className)}>{children}</section>
}

export function PanelHead({
  title,
  sub,
  actions,
  className,
}: {
  title: ReactNode
  sub?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <header className={cx('panel-head', className)}>
      <h2 className="text-md font-semibold text-ink-0">{title}</h2>
      {sub !== undefined ? <span className="text-xs text-ink-2 num">{sub}</span> : null}
      <div className="ml-auto flex items-center gap-2">{actions}</div>
    </header>
  )
}

/* ---------------- 状态芯片 ---------------- */

/**
 * 状态芯片：色相（CSS [data-state]）+ 形状（图标）双编码。
 * kind 决定用执行词表还是节点词表（EXECUTING/SKIPPED 仅节点域）。
 */
export function StatusChip({
  status,
  kind = 'exec',
  className,
  showIcon = true,
}: {
  status: string
  kind?: 'exec' | 'node'
  className?: string
  showIcon?: boolean
}) {
  const meta = kind === 'node' ? nodeStatus(status) : execStatus(status)
  return (
    <span className={cx('chip', className)} data-state={status} title={status}>
      {showIcon ? <I glyph={meta.glyph} size={11} /> : null}
      {meta.label}
    </span>
  )
}

/** 中性元数据芯片：节点类型、触发来源、作用域——不承载状态。 */
export function Badge({
  icon,
  children,
  className,
}: {
  icon?: LucideIcon
  children: ReactNode
  className?: string
}) {
  return (
    <span className={cx('chip-quiet', className)}>
      {icon !== undefined ? <I glyph={icon} size={11} /> : null}
      {children}
    </span>
  )
}

/* ---------------- 空态 / 骨架 / 提示 ---------------- */

export function EmptyState({
  icon,
  title,
  hint,
  action,
}: {
  icon?: LucideIcon
  title: string
  hint?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      {icon !== undefined ? (
        <span className="text-ink-3">
          <I glyph={icon} size={22} />
        </span>
      ) : null}
      <p className="empty-title">{title}</p>
      {hint !== undefined ? <p className="measure text-xs">{hint}</p> : null}
      {action !== undefined ? <div className="mt-1">{action}</div> : null}
    </div>
  )
}

export function Skeleton({ width = '100%', height = 12 }: { width?: string | number; height?: number }) {
  return <span className="skeleton block" style={{ width, height }} aria-hidden />
}

export function Alert({
  tone = 'neutral',
  icon,
  children,
  actions,
}: {
  tone?: 'neutral' | 'error' | 'warn'
  icon?: LucideIcon
  children: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="alert" data-tone={tone} role={tone === 'error' ? 'alert' : undefined}>
      {icon !== undefined ? <I glyph={icon} size={14} className="mt-0.5 shrink-0" /> : null}
      <div className="min-w-0 flex-1">{children}</div>
      {actions !== undefined ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  )
}

/* ---------------- 键值行 ---------------- */

export function KV({ k, children, mono = false }: { k: string; children: ReactNode; mono?: boolean }) {
  return (
    <div className="flex min-w-0 items-baseline gap-3 py-1">
      <dt className="w-24 shrink-0 text-xs text-ink-2">{k}</dt>
      <dd className={cx('min-w-0 flex-1 text-sm text-ink-0', mono && 'mono')}>{children}</dd>
    </div>
  )
}
