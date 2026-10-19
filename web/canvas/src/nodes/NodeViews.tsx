// 五类节点的画布视图:共享外壳(类型 + 标题 + 方向/语义 Handle)+ 各类型摘要行。
// 渲染纪律:全部走 React 文本节点(自动转义),标题等作者自由文本绝不进
// innerHTML(同审批台 textContent 契约,D-t44)。
//
// 编码分工(与执行面共用同一张卡片):
//   - 类型 = 形状:nodeTypeMeta() 的图标 + 微标签;branch 另给折角轮廓。
//   - 状态 = 色相 + 形状:data.status 在场(执行面注入)时渲染
//     <StatusChip kind="node">(状态图标 + 状态色相 1px 边框 + 挂起斜纹);
//     编辑面不注入 status,因此不虚构执行状态。
//   - 未配置 = 亮度/发丝线:虚线边框 + 更暗的待填占位,不占状态色相。
//   - 出口 = 锚点(T21.4 ③):多出口节点(branch)每个锚点一个可连手柄,
//     沿卡片下沿均分排布,id 与卡片内的端口行一一对应;单出口节点保持
//     四向物理手柄,不扩业务出口。锚点集合由 sourceAnchors() 从 spec 推导。
//
// 执行面注入契约(可选字段,缺省 = 编辑面形态):
//   data: { spec: DslNode; status?: string; instanceNo?: number }
import { Handle, Position, type NodeProps } from '@xyflow/react'
import type { ReactNode } from 'react'
import type { DslNode } from '../model/dsl.ts'
import { sideHandle, sourceAnchors, type SourceAnchor } from '../model/convert.ts'
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

const SIDE_LABELS = { top: '上侧', right: '右侧', bottom: '下侧', left: '左侧' } as const

/** 同一侧输入/输出错位,避免两个可拖点覆盖;缺省手柄仍排在各自类型首位。 */
function DirectionHandles({ type }: { type: 'source' | 'target' }) {
  const sides = type === 'source' ? [Position.Bottom, Position.Top, Position.Right, Position.Left] :
    [Position.Top, Position.Right, Position.Bottom, Position.Left]
  return sides.map((side) => (
    <Handle
      key={side}
      id={sideHandle(type, side)}
      type={type}
      position={side}
      className="bn-direction-handle"
      data-side={side}
      title={`${SIDE_LABELS[side]}${type === 'source' ? '输出' : '输入'}连接点`}
      aria-label={`${SIDE_LABELS[side]}${type === 'source' ? '输出' : '输入'}连接点`}
    />
  ))
}

/**
 * 源锚点手柄:空集 = 普通单出口节点的四向物理手柄;
 * 非空 = 一个无 id 兜底手柄 + 每个锚点一个带 id 的手柄,沿下沿均分。
 *
 * **DOM 顺序有语义**:React Flow 解析边端点时,`sourceHandle` 缺省(null)取的是
 * **第一个**源手柄,而不是「没有 id 的那个」—— @xyflow/system 的 getHandle:
 * `handleId ? handles.find(h => h.id === handleId) : handles[0]`,而 handles 按
 * `nodeElement.querySelectorAll('.source')` 的 DOM 序采集。所以兜底手柄必须
 * 渲染在锚点**之前**,否则目标不在 cases∪{default} 内的边(外部/历史文档)
 * 会被当成第一个锚点 case_0 的连线(独立复核 MEDIUM-1,Chromium 实测复现)。
 * 视觉位置与 DOM 顺序解耦(端点按测量坐标画):兜底点仍钉在最右端,不与均分位重叠。
 */
function SourceHandles({ anchors }: { anchors: SourceAnchor[] }) {
  if (anchors.length === 0) return <DirectionHandles type="source" />
  return (
    <>
      <Handle type="source" position={Position.Bottom} style={{ left: '100%' }} data-anchor="fallback" data-side="bottom" />
      {anchors.map((a, i) => (
        <Handle
          key={a.id}
          id={a.id}
          type="source"
          position={Position.Bottom}
          style={{ left: `${((i + 1) * 100) / (anchors.length + 1)}%` }}
          data-anchor={a.id}
          data-side="bottom"
        />
      ))}
    </>
  )
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
      <DirectionHandles type="target" />
      <SourceHandles anchors={sourceAnchors(spec)} />
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
      <div className={cx('bn-line', (spec.model ?? '').trim() === '' ? 'bn-todo' : undefined)}>{(spec.model ?? '').trim() === '' ? '模型未配置' : spec.model}</div>
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
  // 端口行与卡片下沿的锚点手柄同集合、同序(单一推导源 sourceAnchors):
  // 行文本里的「待填」既是占位文案,也标出还没接线的出口。
  return (
    <NodeShell spec={spec} status={statusOf(data)} instanceNo={instanceOf(data)}>
      <ul className="bn-ports">
        {sourceAnchors(spec).map((a) => (
          <li key={a.id} className={cx('bn-port', a.blank && 'bn-todo')} data-port={a.id}>
            {a.label}
          </li>
        ))}
      </ul>
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
