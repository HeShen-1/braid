// 五类节点的画布视图:共享外壳(类型 + 标题 + 上下 Handle)+ 各类型摘要行。
// 渲染纪律:全部走 React 文本节点(自动转义),标题等作者自由文本绝不进
// innerHTML(同审批台 textContent 契约,D-t44)。
//
// 编码分工(与执行面共用同一张卡片):
//   - 类型 = 形状:nodeTypeMeta() 的图标 + 微标签;branch 另给折角轮廓。
//   - 状态 = 色相 + 形状:data.status 在场(执行面注入)时渲染
//     <StatusChip kind="node">(状态图标 + 状态色相 1px 边框 + 挂起斜纹);
//     编辑面不注入 status,因此不虚构执行状态。
//   - 未配置 = 亮度/发丝线:虚线边框 + 更暗的待填占位,不占状态色相。
//
// 执行面注入契约(可选字段,缺省 = 编辑面形态):
//   data: { spec: DslNode; status?: string; instanceNo?: number }
import { Handle, Position, type NodeProps } from '@xyflow/react'
import type { ReactNode } from 'react'
import type { DslNode } from '../model/dsl.ts'
import { nodeStatus, nodeTypeMeta } from '../ui/status.ts'
import { StatusChip } from '../ui/primitives.tsx'
import { I } from '../ui/I.tsx'
import { tid } from '../ui/testids.ts'
import { cx } from '../ui/cx.ts'

function statusOf(data: Record<string, unknown>): string | undefined {
  return typeof data.status === 'string' && data.status !== '' ? data.status : undefined
}

function instanceOf(data: Record<string, unknown>): number | undefined {
  const n = data.instanceNo
  return typeof n === 'number' && Number.isFinite(n) ? n : undefined
}

/** 未配置 = 该类型的必填位还空着(本地可见的作者事实,不是校验裁定)。 */
function isUnconfigured(spec: DslNode): boolean {
  switch (spec.type) {
    case 'llm':
      return spec.prompt.template.trim() === ''
    case 'tool':
      return spec.tool.trim() === ''
    case 'branch':
      return (spec.cases ?? []).every((c) => c.next.trim() === '') && spec.default.trim() === ''
    case 'approval':
      return spec.approvers.length === 0
    case 'parallel':
      return spec.parallel.foreach.trim() === ''
  }
}

export function NodeShell({
  spec,
  status,
  instanceNo,
  children,
}: {
  spec: DslNode
  status?: string
  instanceNo?: number
  children?: ReactNode
}) {
  const meta = nodeTypeMeta(spec.type)
  const blank = isUnconfigured(spec)
  const label = spec.title || spec.id
  // 悬停补全截断文本,并说明虚线边框的来由(未配置/状态)。
  const hint = [label, status !== undefined ? nodeStatus(status).label : null, blank ? '尚未配置' : null]
    .filter((s) => s !== null)
    .join(' · ')
  return (
    <div
      className="bn"
      data-type={spec.type}
      data-blank={blank ? 'true' : undefined}
      data-state={status}
      title={hint}
      data-testid={instanceNo === undefined ? tid.nodeCard(spec.id) : tid.nodeRow(spec.id, instanceNo)}
    >
      <div className="bn-head">
        <I glyph={meta.glyph} size={12} className="text-ink-2" />
        <span className="bn-type">{meta.label}</span>
        <span className="ml-auto flex items-center gap-1.5">
          {instanceNo !== undefined ? <span className="num text-micro text-ink-2">#{instanceNo}</span> : null}
          {status !== undefined ? <StatusChip kind="node" status={status} /> : null}
        </span>
      </div>
      <div className="bn-title">{label}</div>
      {spec.title !== undefined && spec.title !== '' && spec.title !== spec.id ? (
        <div className="bn-id">{spec.id}</div>
      ) : null}
      {children !== undefined ? <div className="bn-body">{children}</div> : null}
      <Handle type="target" position={Position.Top} />
      <Handle type="source" position={Position.Bottom} />
    </div>
  )
}

function refTail(ref: string): string {
  // {{nodes.a.output.b}} → a.output.b;非引用原文显示。
  return ref.startsWith('{{') && ref.endsWith('}}') ? ref.slice(2, -2) : ref
}

function firstLine(s: string | undefined, max = 42): string {
  if (!s) return ''
  const line = s.split('\n')[0]
  return line.length > max ? line.slice(0, max - 1) + '…' : line
}

export function LlmView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'llm' }>
  const template = firstLine(spec.prompt?.template)
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <div className="bn-line">{spec.model || '默认模型'}</div>
      <div className={cx('bn-line', template === '' ? 'bn-todo' : 'bn-dim')}>{template || '待填提示词'}</div>
    </NodeShell>
  )
}

export function ToolView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'tool' }>
  const keys = Object.keys(spec.inputs ?? {})
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <div className={cx('bn-line', spec.tool === '' ? 'bn-todo' : undefined)}>{spec.tool || '待选工具'}</div>
      <div className="bn-line bn-dim num">{keys.length > 0 ? keys.join(', ') : '无入参'}</div>
    </NodeShell>
  )
}

export function BranchView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'branch' }>
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <div className="bn-line num">{spec.cases?.length ?? 0} 个分支</div>
      <div className={cx('bn-line', spec.default === '' ? 'bn-todo' : 'bn-dim')}>默认 → {spec.default || '待填'}</div>
    </NodeShell>
  )
}

export function ApprovalView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'approval' }>
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <div className={cx('bn-line', spec.approvers?.length ? undefined : 'bn-todo')}>
        {spec.approvers?.length ? spec.approvers.join(', ') : '待选审批人'}
      </div>
      <div className="bn-line bn-dim num">
        {spec.mode === 'all' ? '会签' : '或签'} · {spec.timeout_hours ?? 72}h
      </div>
    </NodeShell>
  )
}

export function ParallelView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'parallel' }>
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <div className={cx('bn-line', spec.parallel?.foreach ? undefined : 'bn-todo')}>
        foreach {spec.parallel?.foreach ? refTail(spec.parallel.foreach) : '待填'}
      </div>
      <div className="bn-line bn-dim num">
        body {nodeTypeMeta(spec.parallel?.body.type === 'tool' ? 'tool' : 'llm').label} ·{' '}
        {spec.parallel?.on_failure || 'fail_fast'}
      </div>
    </NodeShell>
  )
}

export const nodeTypes = {
  llm: LlmView,
  tool: ToolView,
  branch: BranchView,
  approval: ApprovalView,
  parallel: ParallelView,
}
