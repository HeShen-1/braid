// 执行时间带(Execution Ribbon)—— T21.1 的 signature move。
//
// 每个节点一行,横向按**真实时间**线性铺开;审批挂起段用 135° 斜纹填充
// (index.css 的 .ribbon-seg[data-state='WAITING_APPROVAL'])。等待在图上
// 是有物理长度的东西:资源没被占用,但时间在走。这是 braid「挂起不占
// worker」机制的可见形态 —— 它不是进度条,也不是 sparkline。
//
// 时间映射(单一真源 = 纯函数 ribbonModel,可单测):
//   from = min(执行 started_at, 全部节点 started_at)
//   to   = 进行中 ? max(now, 最新 finished_at) : 最新 finished_at
//   行内段 = [max(started_at, from), finished_at ?? (进行中 ? now : to)]
// 终态执行的右边界冻结在最后一次结束时刻:否则 now 会一路右移,历史执行
// 被压成左侧一条缝。进行中执行的右边界 = now,由 ui/time 的 useNow 每秒推进。
//
// 可读性:状态色相 + 形状双编码由 StatusChip 承担(中文标签 + 图标),色块
// 只承担"长度";悬停段给完整绝对时刻与时长(title + aria-label 同一文本)。
import { Clock, CircleAlert, ListTree } from 'lucide-react'
import type { NodeExecution } from './api.ts'
import { I } from './ui/I.tsx'
import { Badge, EmptyState, PanelHead, StatusChip } from './ui/primitives.tsx'
import { nodeStatus, nodeTypeMeta } from './ui/status.ts'
import { TID, tid } from './ui/testids.ts'
import { fmtDateTime, fmtDuration, useNow } from './ui/time.tsx'

/** 时间带需要的节点字段(NodeExecution 的子集,测试可裸造)。 */
export type RibbonNode = Pick<
  NodeExecution,
  | 'node_id'
  | 'instance_no'
  | 'type'
  | 'status'
  | 'attempts'
  | 'max_attempts'
  | 'error_code'
  | 'error_message'
  | 'started_at'
  | 'finished_at'
>

export interface RibbonSegment {
  startMs: number
  endMs: number
  /** 相对轨道左端的百分比(0-100)。 */
  left: number
  width: number
  /** 未结束(进行中)= 右端跟着 now 走。 */
  ongoing: boolean
}

export interface RibbonRow {
  key: string
  nodeId: string
  instanceNo: number
  type: string
  status: string
  attempts: number
  maxAttempts: number
  errorCode: string | null
  errorMessage: string | null
  seg: RibbonSegment | null
  durationMs: number | null
  /** 悬停/读屏文本:节点 ID、状态、起止绝对时刻、时长、错误。 */
  title: string
}

export interface RibbonTick {
  left: number
  label: string
  title: string
  iso: string
}

export interface RibbonModel {
  from: number | null
  to: number | null
  live: boolean
  /** now 落在区间内部时的百分比(右边界 = now 时为空:不要在边界上再画一根线)。 */
  nowLeft: number | null
  rows: RibbonRow[]
  ticks: RibbonTick[]
  /** 中间刻度的网格发丝线(仅 5 刻度时)。 */
  gridlines: number[]
  /** 全程时长(ms)。 */
  durationMs: number | null
  /** 挂起(WAITING_APPROVAL)段总时长 —— 本产品最该被看见的数字。 */
  waitMs: number
}

/** 短区间给 3 个刻度(标签短),中段给 5 个;超长区间标签带日期,回到 3 个
 * 避免在窄窗口里互相挤压 —— 密度随执行时长双向自适应(surface-brief Unresolved)。 */
const THREE_TICK_MAX_MS = 120_000
const THREE_TICK_MIN_MS = 6 * 3600_000
/** 秒级以下区间补毫秒,否则起点/中点/终点三个刻度会撞成同一串。 */
const MS_PRECISION_MAX_MS = 10_000

function parseMs(iso: string | null): number | null {
  if (iso === null || iso === '') return null
  const t = new Date(iso).getTime()
  return Number.isNaN(t) ? null : t
}

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

/** 刻度标签:只给绝对时刻;跨天补月-日,亚秒区间补毫秒。完整时刻进 title。 */
function fmtTick(ms: number, withMs: boolean, withDate: boolean): string {
  const d = new Date(ms)
  const clock = `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}${
    withMs ? `.${String(d.getMilliseconds()).padStart(3, '0')}` : ''
  }`
  return withDate ? `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${clock}` : clock
}

/**
 * 时间带模型:全部几何在这一个纯函数里(组件只负责画)。
 * now 由调用方注入 —— 终态执行不读 now,历史帧才是稳定的。
 */
export function ribbonModel(
  nodes: RibbonNode[],
  opts: { startedAt: string | null; finishedAt: string | null; live: boolean; now: number },
): RibbonModel {
  const starts = [opts.startedAt, ...nodes.map((n) => n.started_at)]
    .map(parseMs)
    .filter((t): t is number => t !== null)
  const fins = [opts.finishedAt, ...nodes.map((n) => n.finished_at)]
    .map(parseMs)
    .filter((t): t is number => t !== null)

  if (starts.length === 0) {
    // 一个节点都还没开始:空轨 + 状态芯片,不给坐标轴(不许编时间)。
    return {
      from: null,
      to: null,
      live: opts.live,
      nowLeft: null,
      rows: nodes.map((n) => bareRow(n)),
      ticks: [],
      gridlines: [],
      durationMs: null,
      waitMs: 0,
    }
  }

  const from = Math.min(...starts)
  const maxFin = fins.length > 0 ? Math.max(...fins) : from
  const to = opts.live ? Math.max(opts.now, maxFin) : maxFin
  const span = Math.max(to - from, 1)

  const rows: RibbonRow[] = nodes.map((n) => {
    const started = parseMs(n.started_at)
    if (started === null) return bareRow(n) // PENDING:不画段
    const startMs = Math.min(Math.max(started, from), to)
    const rawEnd = parseMs(n.finished_at) ?? (opts.live ? opts.now : to)
    const endMs = Math.min(Math.max(rawEnd, startMs), to)
    const seg: RibbonSegment = {
      startMs,
      endMs,
      left: ((startMs - from) / span) * 100,
      width: ((endMs - startMs) / span) * 100,
      ongoing: n.finished_at === null,
    }
    const durationMs = endMs - startMs
    return {
      ...bareRow(n),
      seg,
      durationMs,
      title: rowTitle(n, seg, durationMs, n.finished_at !== null),
    }
  })

  const fracs = span < THREE_TICK_MAX_MS || span >= THREE_TICK_MIN_MS ? [0, 0.5, 1] : [0, 0.25, 0.5, 0.75, 1]
  const sameDay = new Date(from).toDateString() === new Date(to).toDateString()
  const withMs = span < MS_PRECISION_MAX_MS
  const ticks: RibbonTick[] = fracs.map((f) => {
    const ms = Math.round(from + span * f)
    return { left: f * 100, label: fmtTick(ms, withMs, !sameDay), title: fmtDateTime(new Date(ms).toISOString()), iso: new Date(ms).toISOString() }
  })

  const waitMs = rows.reduce((acc, r) => (r.status === 'WAITING_APPROVAL' && r.seg !== null ? acc + (r.seg.endMs - r.seg.startMs) : acc), 0)

  return {
    from,
    to,
    live: opts.live,
    nowLeft: opts.live && opts.now < to ? ((opts.now - from) / span) * 100 : null,
    rows,
    ticks,
    gridlines: fracs.length === 5 ? [25, 50, 75] : [],
    durationMs: to - from,
    waitMs,
  }
}

function bareRow(n: RibbonNode): RibbonRow {
  return {
    key: `${n.node_id}#${n.instance_no}`,
    nodeId: n.node_id,
    instanceNo: n.instance_no,
    type: n.type,
    status: n.status,
    attempts: n.attempts,
    maxAttempts: n.max_attempts,
    errorCode: n.error_code,
    errorMessage: n.error_message,
    seg: null,
    durationMs: null,
    title: rowTitle(n, null, null, false),
  }
}

function rowTitle(n: RibbonNode, seg: RibbonSegment | null, durationMs: number | null, finished: boolean): string {
  const lines = [`${n.node_id} #${n.instance_no} · ${nodeStatus(n.status).label}`]
  if (seg === null) {
    lines.push('尚未开始')
  } else {
    lines.push(`开始 ${fmtDateTime(n.started_at)}`)
    lines.push(`结束 ${finished ? fmtDateTime(n.finished_at) : '进行中'}`)
    if (durationMs !== null) lines.push(`时长 ${fmtDuration(durationMs)}`)
  }
  if (n.error_code !== null) lines.push(`错误 ${n.error_code}`)
  return lines.join('\n')
}

/** 图例:实心 = 已发生,斜纹 = 等待审批(时间在走,资源没占)。 */
const LEGEND: Array<{ state: string; label: string }> = [
  { state: 'RUNNING', label: '执行中' },
  { state: 'SUCCEEDED', label: '成功' },
  { state: 'FAILED', label: '失败' },
  { state: 'CANCELLED', label: '取消' },
  { state: 'WAITING_APPROVAL', label: '等待审批' },
]

function Legend() {
  return (
    <div className="exec-ribbon-legend">
      {LEGEND.map((l) => (
        <span key={l.state} className="exec-ribbon-legend-item">
          {/* 复用 .ribbon-seg 的 data-state 配色,避免状态映射第二真源 */}
          <span className="ribbon-seg exec-ribbon-key" data-state={l.state} aria-hidden />
          {l.label}
        </span>
      ))}
    </div>
  )
}

export function ExecutionRibbon({
  nodes,
  startedAt,
  finishedAt,
  live,
  selectedNodeId,
  onSelectNode,
}: {
  nodes: RibbonNode[]
  startedAt: string | null
  finishedAt: string | null
  live: boolean
  selectedNodeId: string | null
  onSelectNode: (nodeId: string) => void
}) {
  // 进行中每秒推进 right = now;终态不读 now(60s 只为满足 hook 无条件调用)。
  const now = useNow(live ? 1000 : 60_000)
  const m = ribbonModel(nodes, { startedAt, finishedAt, live, now })

  return (
    <section className="panel exec-ribbon" data-testid={TID.execRibbon}>
      <PanelHead
        title="执行时间带"
        sub={
          <>
            {`${nodes.length} 个节点`}
            {m.durationMs !== null ? ` · 全程 ${fmtDuration(m.durationMs)}` : ' · 尚未开始'}
            {m.waitMs > 0 ? (
              <>
                {' · '}
                <span className="exec-ribbon-wait">
                  <I glyph={Clock} size={11} />
                  挂起 <span className="num">{fmtDuration(m.waitMs)}</span> · 未占 worker
                </span>
              </>
            ) : null}
          </>
        }
        actions={<Legend />}
      />
      <div className="exec-ribbon-body">
        {nodes.length === 0 ? (
          <EmptyState
            icon={ListTree}
            title="暂无节点执行记录"
            hint="执行推进后,节点会按真实时间铺开在这里;审批挂起段用斜纹标出。"
          />
        ) : (
          <div className="ribbon">
            {m.rows.map((row) => (
              <RibbonRowView
                key={row.key}
                row={row}
                selected={selectedNodeId === row.nodeId}
                onSelect={() => onSelectNode(row.nodeId)}
                gridlines={m.gridlines}
                nowLeft={m.nowLeft}
              />
            ))}
            <div className="ribbon-row exec-axis-row">
              <div className="ribbon-label exec-axis-label micro">时间轴</div>
              <div className="ribbon-axis">
                {m.ticks.length === 0 ? (
                  <span className="text-ink-3">尚未开始</span>
                ) : (
                  m.ticks.map((t) => (
                    <time key={t.left} dateTime={t.iso} title={t.title}>
                      {t.label}
                    </time>
                  ))
                )}
              </div>
            </div>
          </div>
        )}
      </div>
    </section>
  )
}

function RibbonRowView({
  row,
  selected,
  onSelect,
  gridlines,
  nowLeft,
}: {
  row: RibbonRow
  selected: boolean
  onSelect: () => void
  gridlines: number[]
  nowLeft: number | null
}) {
  const type = nodeTypeMeta(row.type)
  const hasError = row.errorCode !== null || row.errorMessage !== null
  return (
    <div
      className="ribbon-row exec-ribbon-row"
      data-testid={tid.nodeRow(row.nodeId, row.instanceNo)}
      data-state={row.status}
      data-selected={selected ? 'true' : 'false'}
      onClick={onSelect}
    >
      <button
        type="button"
        className="ribbon-label"
        onClick={onSelect}
        aria-pressed={selected}
        title={`选择 ${row.nodeId} 作为回滚目标`}
      >
        <StatusChip status={row.status} kind="node" />
        <span className="mono truncate text-ink-0">{row.nodeId}</span>
        <Badge icon={type.glyph}>{type.label}</Badge>
        <span className="num text-ink-2">#{row.instanceNo}</span>
        {row.maxAttempts > 1 ? (
          <span className="num text-ink-2" title="尝试次数 / 最大尝试次数">
            试 {row.attempts}/{row.maxAttempts}
          </span>
        ) : null}
        {row.durationMs !== null ? <span className="num ml-auto text-ink-2">{fmtDuration(row.durationMs)}</span> : null}
      </button>

      <div className="ribbon-track">
        {gridlines.map((left) => (
          <span key={left} className="exec-grid-line" style={{ left: `${left}%` }} aria-hidden />
        ))}
        {row.seg !== null ? (
          <span
            className="ribbon-seg"
            data-state={row.status}
            role="img"
            aria-label={row.title}
            title={row.title}
            style={{ left: `${row.seg.left}%`, width: `${row.seg.width}%` }}
          />
        ) : null}
        {nowLeft !== null ? <span className="ribbon-now" style={{ left: `${nowLeft}%` }} aria-hidden /> : null}
      </div>

      {hasError ? (
        <p className="exec-node-error">
          <I glyph={CircleAlert} size={12} />
          {row.errorCode !== null ? <code>{row.errorCode}</code> : null}
          {row.errorMessage !== null ? <span className="truncate">{row.errorMessage}</span> : null}
        </p>
      ) : null}
    </div>
  )
}
