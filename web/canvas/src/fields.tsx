// 面板字段组件:受控小件,值回写一律走 immutable patch(上层 updateSpec)。
// 布局纪律 = 控制台的纵向扫读:标签永远在控件上方(ui/field 原语负责
// htmlFor 绑定与 aria-invalid),错误紧贴控件下方且优先于 hint 出现。
// 多控件行用 Group —— 它不是 <label>,不会把可及名错误地绑到行内第一个控件。
import type { ReactNode } from 'react'
import { Plus, X } from 'lucide-react'
import { Select, TextArea, TextInput } from './ui/field.tsx'
import { Button } from './ui/primitives.tsx'

/** 字段分组标题(多控件行 / 数组行组):视觉上是 field-label,语义上不是 label。 */
export function Group({ label, hint, children }: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <div className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint !== undefined && hint !== '' ? <p className="field-hint">{hint}</p> : null}
    </div>
  )
}

/** 面板分节:发丝线分隔,微标签做节标题(层级只靠亮度与线,不靠盒子)。 */
export function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="panel-section">
      <header className="panel-section-head">
        <h3 className="micro">{title}</h3>
        {hint !== undefined ? <span className="text-micro text-ink-3">{hint}</span> : null}
      </header>
      {children}
    </section>
  )
}

export function TextField({
  label,
  hint,
  error,
  value,
  onChange,
  placeholder,
  readOnly,
  ariaLabel,
  className,
}: {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  value: string
  onChange: (v: string) => void
  placeholder?: string
  readOnly?: boolean
  /** 无可见标签时给可及名(数组行的键/值列)。 */
  ariaLabel?: string
  className?: string
}) {
  return (
    <TextInput
      label={label}
      hint={hint}
      error={error}
      className={className}
      type="text"
      value={value}
      placeholder={placeholder}
      readOnly={readOnly}
      aria-label={ariaLabel}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

/** 数值字段:min/max 是 schema 边界的镜像(与 BRANCH_OPS 同性质),越界就地报错。 */
export function NumField({
  label,
  hint,
  value,
  onChange,
  step = 1,
  min,
  max,
}: {
  label?: ReactNode
  hint?: ReactNode
  value: number | undefined
  onChange: (v: number | undefined) => void
  step?: number
  min?: number
  max?: number
}) {
  const outOfRange =
    value !== undefined && ((min !== undefined && value < min) || (max !== undefined && value > max))
  return (
    <TextInput
      label={label}
      hint={hint}
      error={outOfRange ? `超出 schema 允许范围（${min ?? '−∞'}–${max ?? '∞'}）` : undefined}
      className="num"
      type="number"
      step={step}
      min={min}
      max={max}
      value={value ?? ''}
      onChange={(e) => {
        if (e.target.value === '') {
          onChange(undefined)
          return
        }
        const n = Number(e.target.value)
        // 输入中途(如 "1e")会得到 NaN:NaN 会被 JSON.stringify 编成 null
        // 违 schema,按"未填"处理比留一个非法中间值干净。
        onChange(Number.isNaN(n) ? undefined : n)
      }}
    />
  )
}

export function AreaField({
  label,
  hint,
  error,
  value,
  onChange,
  rows = 3,
  placeholder,
}: {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  value: string
  onChange: (v: string) => void
  rows?: number
  placeholder?: string
}) {
  return (
    <TextArea
      label={label}
      hint={hint}
      error={error}
      rows={rows}
      value={value}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

export function SelectField({
  label,
  hint,
  error,
  value,
  onChange,
  options,
  ariaLabel,
}: {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string; disabled?: boolean }[]
  /** 无可见标签时给可及名(数组行内的运算符列)。 */
  ariaLabel?: string
}) {
  return (
    <Select
      label={label}
      hint={hint}
      error={error}
      value={value}
      aria-label={ariaLabel}
      onChange={(e) => onChange(e.target.value)}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value} disabled={o.disabled}>
          {o.label}
        </option>
      ))}
    </Select>
  )
}

/**
 * 字面量嗅探:{/[ 开头尝试 JSON.parse,数字/布尔/null 字面量恢复原生
 * 类型,其余(含 {{...}} 引用)原样字符串。tool 入参与 branch 右值共用。
 */
export function parseLiteral(v: string): unknown {
  const t = v.trim()
  if (t === '') return ''
  if (t.startsWith('{') || t.startsWith('[')) {
    try {
      return JSON.parse(t)
    } catch {
      return v
    }
  }
  if (t === 'true' || t === 'false' || t === 'null') return JSON.parse(t)
  if (/^-?\d+(\.\d+)?$/.test(t)) return Number(t)
  return v
}

/** 字符串→字符串映射行(outputs 等):键可改,删行即删键。 */
export function KVRows({
  rows,
  onChange,
  keyLabel,
  valLabel,
}: {
  rows: Record<string, string>
  onChange: (next: Record<string, string>) => void
  keyLabel: string
  valLabel: string
}) {
  const entries = Object.entries(rows)
  const set = (i: number, k: string, v: string) => {
    // 撞键保护(对抗审查):把行 i 改名成已有键会静默吞掉另一行——
    // 拒绝该次改名(行 i 保持原键),数据无损优先于按键就收。
    if (k !== entries[i][0] && entries.some(([ek], j) => j !== i && ek === k)) return
    const next: Record<string, string> = {}
    entries.forEach(([ek, ev], j) => {
      next[j === i ? k : ek] = j === i ? v : ev
    })
    onChange(next)
  }
  return (
    <div className="flex flex-col gap-1.5">
      {entries.map(([k, v], i) => (
        <div className="flex items-center gap-1.5" key={i}>
          <span className="num w-3 shrink-0 text-right text-micro text-ink-3">{i + 1}</span>
          <TextField
            className="min-w-0 flex-1"
            ariaLabel={`${keyLabel} ${i + 1}`}
            value={k}
            onChange={(nv) => set(i, nv, v)}
          />
          <TextField
            className="min-w-0 flex-1"
            ariaLabel={`${valLabel} ${i + 1}`}
            value={v}
            onChange={(nv) => set(i, k, nv)}
          />
          <Button
            size="sm"
            variant="quiet"
            iconOnly
            icon={X}
            aria-label={`删除 ${keyLabel} ${i + 1}`}
            onClick={() => {
              const next = { ...rows }
              delete next[k]
              onChange(next)
            }}
          />
        </div>
      ))}
      {/* 空键行在场时 no-op:spread 覆写会清掉该行已输入的值(对抗审查)。 */}
      <Button
        size="sm"
        variant="quiet"
        icon={Plus}
        className="self-start"
        onClick={() => {
          if (!('' in rows)) onChange({ ...rows, '': '' })
        }}
      >
        添加
      </Button>
    </div>
  )
}

/** 对象数组行编辑(user_data / branch cases):renderRow 定制字段,空对象增删。 */
export function ListRows<T>({
  items,
  onChange,
  make,
  rowLabel,
  renderRow,
}: {
  items: T[]
  onChange: (next: T[]) => void
  make: () => T
  rowLabel: string
  renderRow: (item: T, patch: (p: Partial<T>) => void, i: number) => ReactNode
}) {
  const patchAt = (i: number, p: Partial<T>) => {
    onChange(items.map((it, j) => (j === i ? { ...it, ...p } : it)))
  }
  return (
    <div className="flex flex-col gap-2">
      {items.map((item, i) => (
        <div className="flex items-start gap-1.5" key={i}>
          <span className="num mt-1.5 w-3 shrink-0 text-right text-micro text-ink-3">{i + 1}</span>
          <div className="flex min-w-0 flex-1 flex-col gap-1.5">{renderRow(item, (p) => patchAt(i, p), i)}</div>
          <Button
            size="sm"
            variant="quiet"
            iconOnly
            icon={X}
            aria-label={`删除 ${rowLabel} ${i + 1}`}
            onClick={() => onChange(items.filter((_, j) => j !== i))}
          />
        </div>
      ))}
      <Button size="sm" variant="quiet" icon={Plus} className="self-start" onClick={() => onChange([...items, make()])}>
        添加
      </Button>
    </div>
  )
}
