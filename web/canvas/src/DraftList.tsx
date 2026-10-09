// 工作流侧栏:草稿列表(GET /definitions?status=DRAFT)。左侧面板,
// 顶部固定动作,列表滚动,底部计数。
import { FilePlus2, Workflow } from 'lucide-react'
import type { DraftSummary } from './api.ts'
import { Badge, Button, EmptyState } from './ui/primitives.tsx'
import { I } from './ui/I.tsx'
import { Timestamp } from './ui/time.tsx'
import { TID, tid } from './ui/testids.ts'

const STATUS_LABEL: Record<string, string> = {
  DRAFT: '草稿',
  PUBLISHED: '已发布',
  ARCHIVED: '已归档',
}

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
    <nav className="flex w-64 shrink-0 flex-col border-r border-line-1 bg-ground-1" aria-label="工作流列表" data-testid={TID.draftList}>
      <div className="flex h-11 shrink-0 items-center gap-2 border-b border-line-0 px-3">
        <span className="micro">工作流</span>
        <span className="num text-xs text-ink-2">{drafts.length}</span>
        <Button
          size="sm"
          variant="quiet"
          icon={FilePlus2}
          className="ml-auto"
          onClick={onNew}
          data-testid={TID.draftNew}
        >
          新建
        </Button>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {drafts.length === 0 ? (
          <div data-testid={TID.draftEmpty}>
            <EmptyState
              icon={Workflow}
              title="还没有工作流"
              hint="新建一个草稿，用画布把节点连成 DAG，发布后即可触发执行。"
              action={
                <Button size="sm" icon={FilePlus2} onClick={onNew}>
                  新建工作流
                </Button>
              }
            />
          </div>
        ) : (
          <ul className="flex flex-col">
            {drafts.map((d) => {
              const active = activeId === d.id
              return (
                <li key={d.id}>
                  <button
                    type="button"
                    className={`flex w-full flex-col gap-1 border-b border-line-0 px-3 py-2 text-left transition-colors ${
                      active ? 'bg-ground-3' : 'hover:bg-ground-2'
                    }`}
                    data-testid={tid.draftItem(d.id)}
                    aria-current={active ? 'true' : undefined}
                    onClick={() => onOpen(d.id)}
                  >
                    <span className="flex w-full items-center gap-2">
                      <I glyph={Workflow} size={12} className={active ? 'text-accent' : 'text-ink-2'} />
                      <span className={`min-w-0 flex-1 truncate text-sm ${active ? 'font-semibold text-ink-0' : 'text-ink-0'}`}>
                        {d.name}
                      </span>
                    </span>
                    <span className="flex w-full items-center gap-2">
                      <Badge>{STATUS_LABEL[d.status] ?? d.status}</Badge>
                      {d.version !== null ? <span className="num text-micro text-ink-2">v{d.version}</span> : null}
                      <span className="ml-auto text-micro">
                        <Timestamp iso={d.updated_at} />
                      </span>
                    </span>
                  </button>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </nav>
  )
}
