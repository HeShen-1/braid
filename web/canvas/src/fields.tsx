// 面板字段组件:受控小件,值回写一律走 immutable patch(上层 updateSpec)。
import type { ReactNode } from 'react'

export function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="f-row">
      <span className="f-label">{label}</span>
      <span className="f-input">{children}</span>
    </label>
  )
}

export function TextField({
  value,
  onChange,
  placeholder,
}: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
}) {
  return <input className="f-text" type="text" value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />
}

export function NumField({
  value,
  onChange,
  step = 1,
}: {
  value: number | undefined
  onChange: (v: number | undefined) => void
  step?: number
}) {
  return (
    <input
      className="f-text"
      type="number"
      step={step}
      value={value ?? ''}
      onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))}
    />
  )
}

export function AreaField({
  value,
  onChange,
  rows = 3,
  placeholder,
}: {
  value: string
  onChange: (v: string) => void
  rows?: number
  placeholder?: string
}) {
  return (
    <textarea
      className="f-area"
      rows={rows}
      value={value}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

export function SelectField({
  value,
  onChange,
  options,
}: {
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string; disabled?: boolean }[]
}) {
  return (
    <select className="f-select" value={value} onChange={(e) => onChange(e.target.value)}>
      {options.map((o) => (
        <option key={o.value} value={o.value} disabled={o.disabled}>
          {o.label}
        </option>
      ))}
    </select>
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
    <div className="f-kv">
      {entries.map(([k, v], i) => (
        <div className="f-kv-row" key={i}>
          <input
            className="f-text"
            type="text"
            aria-label={`${keyLabel} ${i + 1}`}
            value={k}
            onChange={(e) => set(i, e.target.value, v)}
          />
          <input
            className="f-text"
            type="text"
            aria-label={`${valLabel} ${i + 1}`}
            value={v}
            onChange={(e) => set(i, k, e.target.value)}
          />
          <button
            type="button"
            className="f-x"
            aria-label={`删除 ${keyLabel} ${i + 1}`}
            onClick={() => {
              const next = { ...rows }
              delete next[k]
              onChange(next)
            }}
          >
            ×
          </button>
        </div>
      ))}
      {/* 空键行在场时 no-op:spread 覆写会清掉该行已输入的值(对抗审查)。 */}
      <button
        type="button"
        className="f-add"
        onClick={() => {
          if (!('' in rows)) onChange({ ...rows, '': '' })
        }}
      >
        + 添加
      </button>
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
    <div className="f-kv">
      {items.map((item, i) => (
        <div className="f-list-row" key={i}>
          {renderRow(item, (p) => patchAt(i, p), i)}
          <button
            type="button"
            className="f-x"
            aria-label={`删除 ${rowLabel} ${i + 1}`}
            onClick={() => onChange(items.filter((_, j) => j !== i))}
          >
            ×
          </button>
        </div>
      ))}
      <button type="button" className="f-add" onClick={() => onChange([...items, make()])}>
        + 添加
      </button>
    </div>
  )
}
