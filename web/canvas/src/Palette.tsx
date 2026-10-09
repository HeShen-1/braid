// palette 工具条:添加节点的唯一入口。类型靠图标形状 + 中文微标签辨认,
// 交互靠亮度(悬停升一档),不占状态色相——色相在这个界面上只讲状态。
import { NODE_TYPES, type NodeType } from './model/dsl.ts'
import { nodeTypeMeta } from './ui/status.ts'
import { I } from './ui/I.tsx'
import { TID, tid } from './ui/testids.ts'

export function Palette({ onAdd }: { onAdd: (type: NodeType) => void }) {
  return (
    <div className="editor-rail">
      <span className="micro pb-1 text-center">添加</span>
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
              data-testid={tid.paletteBtn(t)}
              title={`添加${meta.label}节点`}
              onClick={() => onAdd(t)}
            >
              <I glyph={meta.glyph} size={16} />
              <span className="micro">{meta.label}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}
