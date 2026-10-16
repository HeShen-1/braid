// 待办审批队列(T21.2 Lane A)——面板:待办表 + 翻页 + 刷新。
//
// 与旧 /ui/ 审批台(app.js renderPending)逐列等价:工作流 `name vX` /
// 执行 ID / 节点 / 截止时间 / 处理;T21.4 增项加「已等待」列(requested_at
// → now,既有 Duration 组件,零新端点)。打开动作由**整行**承担(行可点、
// Enter/Space 等效),行内「处理」按钮保留"这一列是动作列"的读法 ——
// 两条路径都只调 onOpen,选中态真源在 URL(路由),组件不持有第二份。
//
// 键盘光标(cursor)与选中(selected)是两件事:光标 = 下一次 Enter 的目标,
// 选中 = 已经打开的详情。j/k 只动光标,不动 URL。
import { Inbox, RefreshCw, TriangleAlert } from 'lucide-react'
import { useEffect, useRef } from 'react'
import type { PendingApproval } from './api.ts'
import { Alert, Button, EmptyState, PanelHead } from './ui/primitives.tsx'
import { TID, tid } from './ui/testids.ts'
import { Duration, Timestamp } from './ui/time.tsx'

/** 一页 20 条(旧实现的 PAGE_LIMIT,服务端上限内)。 */
export const PAGE_LIMIT = 20

/** 队列行 → 详情 的选择;路由用同一对 id 拼 URL。 */
export interface ApprovalSelection {
  executionId: string
  nodeId: string
}

/** 已过截止(旧实现 td.overdue 的等价物)。 */
function overdue(iso: string): boolean {
  if (iso === '') return false
  const t = new Date(iso).getTime()
  return !Number.isNaN(t) && t < Date.now()
}

export function ApprovalQueue({
  items,
  total,
  offset,
  loading,
  error,
  flash,
  selected,
  cursor,
  focusRequest = 0,
  lastDecision = null,
  onOpen,
  onRefresh,
  onPrev,
  onNext,
}: {
  items: PendingApproval[]
  total: number
  offset: number
  loading: boolean
  error: string
  flash: string
  selected: ApprovalSelection | null
  cursor: number
  focusRequest?: number
  lastDecision?: ApprovalSelection | null
  onOpen: (sel: ApprovalSelection) => void
  onRefresh: () => void
  onPrev: () => void
  onNext: () => void
}) {
  // 与旧实现同口径:上一页 = offset>0;下一页 = offset+PAGE_LIMIT < total。
  const hasPrev = offset > 0
  const hasNext = offset + PAGE_LIMIT < total
  const rootRef = useRef<HTMLElement | null>(null)
  const consumedFocus = useRef(0)
  useEffect(() => {
    if (focusRequest === 0 || focusRequest === consumedFocus.current || loading) return
    const rows = rootRef.current?.querySelectorAll<HTMLTableRowElement>('tbody tr')
    const row = rows?.[Math.min(cursor, (rows?.length ?? 1) - 1)]
    const target = row ?? rootRef.current
    target?.focus({ preventScroll: true })
    row?.scrollIntoView?.({ block: 'nearest' })
    consumedFocus.current = focusRequest
  }, [cursor, focusRequest, items, loading])

  return (
    <aside ref={rootRef} tabIndex={-1} className="appr-rail" aria-label="待办审批队列">
      <section className="panel" data-testid={TID.approvalQueue} aria-busy={loading}>
        <PanelHead
          title="待办审批"
          sub={total > items.length ? `${items.length} / ${total} 条` : `${items.length} 条`}
          actions={
            <Button
              variant="quiet"
              size="sm"
              iconOnly
              icon={RefreshCw}
              aria-label="刷新待办队列"
              data-testid={TID.approvalRefresh}
              disabled={loading}
              onClick={onRefresh}
            />
          }
        />

        {flash !== '' ? (
          <p className="appr-flash" role="status" data-testid={TID.approvalFlash}>
            {flash}
            {lastDecision !== null ? <Button size="sm" variant="quiet" onClick={() => onOpen(lastDecision)}>查看刚才的决定</Button> : null}
          </p>
        ) : null}

        {error !== '' ? (
          <div className="appr-error">
            <Alert tone="error" icon={TriangleAlert}>
              {error}
            </Alert>
          </div>
        ) : null}

        {items.length === 0 ? (
          <div data-testid={TID.approvalEmpty}>
            {loading ? (
              <p className="panel-body text-sm text-ink-2">加载待办审批…</p>
            ) : error !== '' ? (
              // 读失败时不知道队列里有什么 —— 不冒充「没有待办」(诚实原则)。
              <p className="panel-body text-sm text-ink-2">待办队列未能读取 —— 见上方错误。</p>
            ) : (
              <EmptyState
                icon={Inbox}
                title="当前没有待办审批"
                hint="挂起的执行会出现在这里:看清卡在哪个节点、已经等了多久,再批准或驳回。"
              />
            )}
          </div>
        ) : (
          <>
            <div className="appr-table-wrap">
              <table className="table" role="grid" aria-label="待办审批">
                <thead>
                  <tr>
                    <th>工作流</th>
                    <th>执行 ID</th>
                    <th>节点</th>
                    <th>已等待</th>
                    <th>截止时间</th>
                    <th>
                      <span className="sr-only">处理</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((it, i) => {
                    const isSelected =
                      selected !== null && selected.executionId === it.execution_id && selected.nodeId === it.node_id
                    const open = () => onOpen({ executionId: it.execution_id, nodeId: it.node_id })
                    const late = overdue(it.deadline_at)
                    return (
                      <tr
                        key={`${it.execution_id}/${it.node_id}`}
                        data-testid={tid.approvalRow(it.execution_id, it.node_id)}
                        data-selected={isSelected ? 'true' : 'false'}
                        aria-selected={isSelected}
                        data-cursor={i === cursor ? 'true' : 'false'}
                        tabIndex={0}
                        title={`#${it.execution_id} · ${it.definition_name} v${it.definition_version} · ${it.node_id}`}
                        onClick={open}
                        onKeyDown={(ev) => {
                          // 行自己吃掉 Enter/Space:全局快捷键靠 defaultPrevented
                          // 让位,避免同一次按键把 onOpen 调两遍。
                          if (ev.key === 'Enter' || ev.key === ' ') {
                            ev.preventDefault()
                            open()
                          }
                        }}
                      >
                        <td>
                          <div className="appr-row-name">
                            <span className="truncate text-ink-0">{it.definition_name}</span>
                            <span className="num text-ink-2">v{it.definition_version}</span>
                          </div>
                        </td>
                        <td className="mono">{it.execution_id}</td>
                        <td>
                          <code className="mono">{it.node_id}</code>
                        </td>
                        {/* 已等待 = requested_at → now(Duration to=null 即算到
                            现在,每秒推进;title 带绝对时刻两面同给) */}
                        <td data-testid={tid.approvalWaited(it.execution_id, it.node_id)}>
                          <Duration from={it.requested_at} to={null} />
                        </td>
                        <td data-overdue={late ? 'true' : 'false'}>
                          <Timestamp iso={it.deadline_at} />
                          {late ? <span className="micro ml-1">已逾期</span> : null}
                        </td>
                        <td className="text-right">
                          <Button
                            variant="quiet"
                            size="sm"
                            onClick={(ev) => {
                              ev.stopPropagation()
                              open()
                            }}
                          >
                            处理
                          </Button>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>

            <div className="appr-pager">
              <span className="num text-ink-2">
                {offset + 1}–{offset + items.length} / {total}
              </span>
              <Button variant="quiet" size="sm" data-testid={TID.approvalPrev} disabled={!hasPrev} onClick={onPrev}>
                上一页
              </Button>
              <Button variant="quiet" size="sm" data-testid={TID.approvalNext} disabled={!hasNext} onClick={onNext}>
                下一页
              </Button>
            </div>
          </>
        )}

        <p className="appr-keys">
          <kbd>j</kbd>
          <kbd>k</kbd> 移动 · <kbd>Enter</kbd> 打开 · <kbd>a</kbd> 批准 · <kbd>r</kbd> 驳回 · <kbd>Esc</kbd> 返回
        </p>
      </section>
    </aside>
  )
}
