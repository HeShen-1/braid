import type { ReactNode } from 'react'

/** 同页工作区标签；方向键切换并聚焦，面板由调用方以 id 配对。 */
export function SectionTabs({ id, label, items, value, onChange, actions }: {
  id: string; label: string; items: { value: string; label: string }[]
  value: string; onChange: (value: string) => void; actions?: ReactNode
}) {
  return <div className="section-tabs-row">
    <div className="section-tabs" role="tablist" aria-label={label}>
      {items.map((item, index) => <button key={item.value} id={`${id}-tab-${item.value}`} type="button"
        role="tab" aria-selected={value === item.value} aria-controls={`${id}-panel-${item.value}`}
        tabIndex={value === item.value ? 0 : -1} onClick={() => onChange(item.value)}
        onKeyDown={(event) => {
          const next = event.key === 'ArrowRight' ? (index + 1) % items.length
            : event.key === 'ArrowLeft' ? (index + items.length - 1) % items.length
            : event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 : -1
          if (next < 0) return
          event.preventDefault()
          onChange(items[next].value)
          event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus()
        }}>{item.label}</button>)}
    </div>{actions}
  </div>
}
