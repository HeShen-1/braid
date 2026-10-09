// 表单原语：标签永远在控件上方（控制台是纵向扫读），错误就近呈现，
// 帮助文本不与错误抢位。所有控件原生可聚焦、可键盘操作。
//
// T21.3 决策 3（单点收敛）：Select / Checkbox 的**外壳**（标签/hint/error 布局、
// id 绑定、调用点 API）仍是自研；**内部控制与键盘行为**换成 shadcn 底层原语
// （Base UI，见 src/components/ui/）。全仓只有这一处实现，不存在第二套。
import { Children, isValidElement, useId, type InputHTMLAttributes, type ReactNode, type TextareaHTMLAttributes } from 'react'
import {
  Select as SelectPrimitive,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Checkbox as CheckboxPrimitive } from '@/components/ui/checkbox'
import { cx } from './cx.ts'

export function Field({
  label,
  hint,
  error,
  children,
  className,
  htmlFor,
}: {
  label: ReactNode
  hint?: ReactNode
  error?: ReactNode
  children: ReactNode
  className?: string
  htmlFor?: string
}) {
  return (
    <div className={cx('field', className)}>
      <label className="field-label" htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {error !== undefined && error !== '' ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : hint !== undefined ? (
        <p className="field-hint">{hint}</p>
      ) : null}
    </div>
  )
}

export function TextInput({
  label,
  hint,
  error,
  className,
  ...rest
}: { label?: ReactNode; hint?: ReactNode; error?: ReactNode } & InputHTMLAttributes<HTMLInputElement>) {
  const auto = useId()
  const id = rest.id ?? auto
  const control = (
    <input id={id} className={cx('input', className)} aria-invalid={error !== undefined && error !== ''} {...rest} />
  )
  if (label === undefined) return control
  return (
    <Field label={label} hint={hint} error={error} htmlFor={id}>
      {control}
    </Field>
  )
}

export function TextArea({
  label,
  hint,
  error,
  className,
  ...rest
}: { label?: ReactNode; hint?: ReactNode; error?: ReactNode } & TextareaHTMLAttributes<HTMLTextAreaElement>) {
  const auto = useId()
  const id = rest.id ?? auto
  const control = (
    <textarea id={id} className={cx('textarea', className)} aria-invalid={error !== undefined && error !== ''} {...rest} />
  )
  if (label === undefined) return control
  return (
    <Field label={label} hint={hint} error={error} htmlFor={id}>
      {control}
    </Field>
  )
}

/**
 * `<option>` 列表 → shadcn Select 的 item 表(App Router 侧调用点因此零改动)。
 * 只认原生 `option`;条件渲染产生的 false/null 由 Children.toArray 清掉。
 * **不支持 `<optgroup>`**(会被静默丢成顶层 option 之外的空白):当前无调用点,
 * 需要时显式扩展,别指望它透明工作。
 *
 * 已知残余(实测,非猜测):打开弹层时 Base UI 会插一个 `<style>` 元素,而静态面
 * CSP 逐字钉死 `style-src 'self'`,该元素被拦(控制台一条 style-src-elem 违规)。
 * 影响为零:定位走 CSSOM(内联 position/--anchor-* 已生效,弹层落点实测正确),
 * 被拦的是组件自带的滚动锁样式。消掉它只能给 CSP 加 hash —— 那要新 D- 条目。
 */
function optionItems(children: ReactNode) {
  const items: { value: string; label: ReactNode; disabled?: boolean }[] = []
  for (const child of Children.toArray(children)) {
    if (!isValidElement(child) || child.type !== 'option') continue
    const props = child.props as { value?: string | number; disabled?: boolean; children?: ReactNode }
    items.push({ value: String(props.value ?? ''), label: props.children, disabled: props.disabled })
  }
  return items
}

export interface SelectProps {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  className?: string
  id?: string
  value?: string
  disabled?: boolean
  'aria-label'?: string
  'data-testid'?: string
  /** 底层不再是原生 select ⇒ 直接回传值,不再有 DOM ChangeEvent。 */
  onChange?: (value: string) => void
  /** 原生 `<option>` 列表(外壳负责翻译成 shadcn item)。 */
  children: ReactNode
}

export function Select({ label, hint, error, className, children, id, value, disabled, onChange, ...rest }: SelectProps) {
  const auto = useId()
  const cid = id ?? auto
  const items = optionItems(children)
  const control = (
    <SelectPrimitive
      value={value ?? ''}
      disabled={disabled}
      // items 让触发器在弹层未挂载时也能显示选中项的标签(Base UI 契约)。
      items={items.map((it) => ({ value: it.value, label: it.label }))}
      onValueChange={(next) => onChange?.(String(next ?? ''))}
    >
      <SelectTrigger id={cid} className={cx('select', className)} {...rest}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {items.map((it, i) => (
          // key 用下标:值可能重复(同名多版本的历史数据),值做 key 会打 React
          // 重复键告警。显示项由调用点保证唯一(见 ExecutionsView 的按名收敛)。
          <SelectItem key={`${i}-${it.value}`} value={it.value} disabled={it.disabled}>
            {it.label}
          </SelectItem>
        ))}
      </SelectContent>
    </SelectPrimitive>
  )
  if (label === undefined) return control
  return (
    <Field label={label} hint={hint} error={error} htmlFor={cid}>
      {control}
    </Field>
  )
}

export interface CheckboxProps {
  label: ReactNode
  hint?: ReactNode
  className?: string
  id?: string
  checked?: boolean
  disabled?: boolean
  /** 同 Select:底层是 Base UI checkbox,回传布尔值而不是 DOM ChangeEvent。 */
  onCheckedChange?: (checked: boolean) => void
}

export function Checkbox({ label, hint, className, id, checked, disabled, onCheckedChange }: CheckboxProps) {
  const auto = useId()
  const cid = id ?? auto
  return (
    <div className={cx('flex items-start gap-2', className)}>
      <CheckboxPrimitive
        id={cid}
        checked={checked}
        disabled={disabled}
        onCheckedChange={(next) => onCheckedChange?.(next)}
        className="mt-0.5"
      />
      <div className="min-w-0">
        <label htmlFor={cid} className="field-label cursor-pointer">
          {label}
        </label>
        {hint !== undefined ? <p className="field-hint">{hint}</p> : null}
      </div>
    </div>
  )
}
