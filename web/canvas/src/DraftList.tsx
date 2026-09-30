// 草稿列表侧栏:GET /definitions?status=DRAFT;点击开草稿进画布。
import type { DraftSummary } from './api.ts'

export function DraftList({
  drafts,
  activeId,
  onOpen,
  onNew,
}: {
  drafts: DraftSummary[]
  activeId: string | null
  onOpen: (id: string) => void
  onNew: () => void
}) {
  return (
    <nav className="draft-list" aria-label="草稿列表">
      <button type="button" className="palette-btn" data-testid="new-draft" onClick={onNew}>
        + 新建工作流
      </button>
      {drafts.length === 0 ? <p className="draft-empty">暂无草稿</p> : null}
      <ul>
        {drafts.map((d) => (
          <li key={d.id}>
            <button
              type="button"
              className={`draft-item${activeId === d.id ? ' active' : ''}`}
              data-testid={`draft-${d.id}`}
              onClick={() => onOpen(d.id)}
            >
              <span className="draft-name">{d.name}</span>
              <span className="draft-time">{d.updated_at}</span>
            </button>
          </li>
        ))}
      </ul>
    </nav>
  )
}
