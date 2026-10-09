// 表单原语：标签永远在控件上方（控制台是纵向扫读），错误就近呈现，
// 帮助文本不与错误抢位。所有控件原生可聚焦、可键盘操作。
import { useId, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'
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

export function Select({
  label,
  hint,
  error,
  className,
  children,
  ...rest
}: { label?: ReactNode; hint?: ReactNode; error?: ReactNode } & SelectHTMLAttributes<HTMLSelectElement>) {
  const auto = useId()
  const id = rest.id ?? auto
  const control = (
    <select id={id} className={cx('select', className)} {...rest}>
      {children}
    </select>
  )
  if (label === undefined) return control
  return (
    <Field label={label} hint={hint} error={error} htmlFor={id}>
      {control}
    </Field>
  )
}

export function Checkbox({
  label,
  hint,
  className,
  ...rest
}: { label: ReactNode; hint?: ReactNode } & InputHTMLAttributes<HTMLInputElement>) {
  const auto = useId()
  const id = rest.id ?? auto
  return (
    <div className={cx('flex items-start gap-2', className)}>
      <input
        id={id}
        type="checkbox"
        className="mt-0.5 size-3.5 shrink-0 rounded-[4px] border border-line-2 bg-ground-2 accent-accent"
        {...rest}
      />
      <div className="min-w-0">
        <label htmlFor={id} className="field-label cursor-pointer">
          {label}
        </label>
        {hint !== undefined ? <p className="field-hint">{hint}</p> : null}
      </div>
    </div>
  )
}
