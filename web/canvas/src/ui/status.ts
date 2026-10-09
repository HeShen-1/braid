// 状态词表：与 DDL ENUM 一一对应（执行 6 值 / 节点 7 值）。
// 色相由 CSS 的 [data-state] 承担，这里只给中文标签与**形状**——
// 状态永不只靠颜色编码（色盲可达）。
import {
  CircleCheck,
  CircleDashed,
  CircleHelp,
  CircleSlash,
  CircleX,
  Clock,
  Cpu,
  GitBranch,
  GitFork,
  LoaderCircle,
  SkipForward,
  UserCheck,
  Wrench,
  type LucideIcon,
} from 'lucide-react'

export type Tone = 'idle' | 'run' | 'ok' | 'wait' | 'fail' | 'stop'

export interface StatusMeta {
  label: string
  tone: Tone
  glyph: LucideIcon
  /** 终态：不再迁移，可安全作为回滚/重跑锚点。 */
  terminal?: boolean
}

const UNKNOWN: StatusMeta = { label: '未知', tone: 'stop', glyph: CircleHelp }

/** 执行状态：migrations/0001 workflow_execution.status ENUM。 */
export const EXEC_STATUS: Record<string, StatusMeta> = {
  PENDING: { label: '待执行', tone: 'idle', glyph: CircleDashed },
  RUNNING: { label: '执行中', tone: 'run', glyph: LoaderCircle },
  WAITING_APPROVAL: { label: '等待审批', tone: 'wait', glyph: Clock },
  SUCCEEDED: { label: '已成功', tone: 'ok', glyph: CircleCheck, terminal: true },
  FAILED: { label: '已失败', tone: 'fail', glyph: CircleX, terminal: true },
  CANCELLED: { label: '已取消', tone: 'stop', glyph: CircleSlash, terminal: true },
}

/** 节点状态：migrations/0001 node_execution.status ENUM（EXECUTING/SKIPPED 仅节点域）。 */
export const NODE_STATUS: Record<string, StatusMeta> = {
  PENDING: { label: '待执行', tone: 'idle', glyph: CircleDashed },
  EXECUTING: { label: '执行中', tone: 'run', glyph: LoaderCircle },
  WAITING_APPROVAL: { label: '等待审批', tone: 'wait', glyph: Clock },
  SUCCEEDED: { label: '已成功', tone: 'ok', glyph: CircleCheck, terminal: true },
  FAILED: { label: '已失败', tone: 'fail', glyph: CircleX, terminal: true },
  SKIPPED: { label: '已跳过', tone: 'stop', glyph: SkipForward, terminal: true },
  CANCELLED: { label: '已取消', tone: 'stop', glyph: CircleSlash, terminal: true },
}

export function execStatus(status: string): StatusMeta {
  return EXEC_STATUS[status] ?? { ...UNKNOWN, label: status }
}

export function nodeStatus(status: string): StatusMeta {
  return NODE_STATUS[status] ?? { ...UNKNOWN, label: status }
}

/** 节点类型（DSL NODE_TYPES）——形状 + 中文标签。 */
export const NODE_TYPE_META: Record<string, { label: string; glyph: LucideIcon }> = {
  llm: { label: 'LLM', glyph: Cpu },
  tool: { label: '工具', glyph: Wrench },
  branch: { label: '分支', glyph: GitBranch },
  approval: { label: '审批', glyph: UserCheck },
  parallel: { label: '并行', glyph: GitFork },
}

export function nodeTypeMeta(type: string): { label: string; glyph: LucideIcon } {
  return NODE_TYPE_META[type] ?? { label: type, glyph: CircleHelp }
}

/** 触发来源（execution.trigger_type ENUM）。 */
export const TRIGGER_LABEL: Record<string, string> = {
  API: '接口触发',
  SYSTEM: '系统触发',
  TEST: '测试触发',
  SCHEDULE: '定时触发',
}

/** 审批决定词表（approval_decision.decision）。 */
export const DECISION_LABEL: Record<string, string> = {
  APPROVED: '批准',
  REJECTED: '驳回',
}

/** 超时策略词表（approval.on_timeout）。 */
export const ON_TIMEOUT_LABEL: Record<string, string> = {
  escalate: '升级通知并继续等待',
  notify: '仅通知',
  fail: '标记失败',
  reject: '自动驳回',
}
