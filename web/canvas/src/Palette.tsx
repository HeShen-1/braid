// palette:添加节点入口。id 生成/默认 spec 见 defaults/convert。
import { NODE_TYPES, type NodeType } from './model/dsl.ts'
import { TYPE_LABELS } from './defaults.ts'

export function Palette({ onAdd }: { onAdd: (type: NodeType) => void }) {
  return (
    <div className="palette" role="toolbar" aria-label="添加节点">
      {NODE_TYPES.map((t) => (
        <button key={t} type="button" className="palette-btn" data-testid={`add-${t}`} onClick={() => onAdd(t)}>
          + {TYPE_LABELS[t]}
        </button>
      ))}
    </div>
  )
}
