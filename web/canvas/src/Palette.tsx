// palette 工具条:添加节点的唯一入口。类型靠彩色线索、图标形状 + 中文微标签辨认,
// 交互靠亮度(悬停升一档),类型色只讲类型,不把执行状态画进编辑面。
import { useEffect, useRef } from 'react'
import { Plus } from 'lucide-react'
import { NODE_TYPES, type NodeType } from './model/dsl.ts'
import { nodeTypeMeta } from './ui/status.ts'
import { I } from './ui/I.tsx'
import { TID, tid } from './ui/testids.ts'

export function Palette({ onAdd }: { onAdd: (type: NodeType) => void }) {
  const menu = useRef<HTMLDetailsElement>(null)
  useEffect(() => {
    const dismiss = (event: PointerEvent) => {
      if (menu.current && !menu.current.contains(event.target as Node)) menu.current.open = false
    }
    document.addEventListener('pointerdown', dismiss)
    return () => document.removeEventListener('pointerdown', dismiss)
  }, [])
  return (
    <details ref={menu} className="editor-palette" onKeyDown={(event) => {
      if (event.key === 'Escape' && menu.current?.open) {
        event.preventDefault()
        menu.current.open = false
        menu.current.querySelector('summary')?.focus()
      }
    }}>
      <summary className="btn btn-sm"><I glyph={Plus} />添加节点</summary>
      <div
        className="flex flex-col gap-0.5"
        role="toolbar"
        aria-label="添加节点"
        aria-orientation="vertical"
        data-testid={TID.palette}
      >
        {NODE_TYPES.map((t) => {
          const meta = nodeTypeMeta(t)
          return (
            <button
              key={t}
              type="button"
              className="palette-btn"
              data-type={t}
              data-testid={tid.paletteBtn(t)}
              title={`添加${meta.label}节点`}
              onClick={() => onAdd(t)}
            >
              <I glyph={meta.glyph} size={16} />
              <span className="micro">{t === 'approval' ? '审批节点' : meta.label}</span>
            </button>
          )
        })}
      </div>
    </details>
  )
}
