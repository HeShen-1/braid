// 五类节点的画布视图:共享外壳(类型徽标 + 标题 + 上下 Handle)+
// 各类型摘要行。渲染纪律:全部走 React 文本节点(自动转义),标题等
// 作者自由文本绝不进 innerHTML(同审批台 textContent 契约,D-t44)。
import { Handle, Position, type NodeProps } from '@xyflow/react'
import type { DslNode } from '../model/dsl.ts'
import { TYPE_LABELS } from '../defaults.ts'

function firstLine(s: string | undefined, max = 42): string {
  if (!s) return ''
  const line = s.split('\n')[0]
  return line.length > max ? line.slice(0, max - 1) + '…' : line
}

export function NodeShell({
  spec,
  children,
}: {
  spec: DslNode
  children?: React.ReactNode
}) {
  return (
    <div className={`bn bn-${spec.type}`}>
      <div className="bn-head">
        <span className="bn-badge">{TYPE_LABELS[spec.type]}</span>
        <span className="bn-title">{spec.title || spec.id}</span>
      </div>
      {children ? <div className="bn-body">{children}</div> : null}
      <Handle type="target" position={Position.Top} className="bn-handle" />
      <Handle type="source" position={Position.Bottom} className="bn-handle" />
    </div>
  )
}

function refTail(ref: string): string {
  // {{nodes.a.output.b}} → a.output.b;非引用原文显示。
  return ref.startsWith('{{') && ref.endsWith('}}') ? ref.slice(2, -2) : ref
}

export function LlmView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'llm' }>
  return (
    <NodeShell spec={spec}>
      <div className="bn-line">{spec.model || '默认模型'}</div>
      <div className="bn-line bn-dim">{firstLine(spec.prompt?.template) || '待填提示词'}</div>
    </NodeShell>
  )
}

export function ToolView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'tool' }>
  return (
    <NodeShell spec={spec}>
      <div className="bn-line">{spec.tool || '待选工具'}</div>
      <div className="bn-line bn-dim">
        {Object.keys(spec.inputs ?? {}).length > 0 ? Object.keys(spec.inputs!).join(', ') : '无入参'}
      </div>
    </NodeShell>
  )
}

export function BranchView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'branch' }>
  return (
    <NodeShell spec={spec}>
      <div className="bn-line">{spec.cases?.length ?? 0} 个分支</div>
      <div className="bn-line bn-dim">默认 → {spec.default || '待填'}</div>
    </NodeShell>
  )
}

export function ApprovalView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'approval' }>
  return (
    <NodeShell spec={spec}>
      <div className="bn-line">{spec.approvers?.length ? spec.approvers.join(', ') : '待选审批人'}</div>
      <div className="bn-line bn-dim">
        {spec.mode === 'all' ? '会签' : '或签'} · {spec.timeout_hours ?? 72}h
      </div>
    </NodeShell>
  )
}

export function ParallelView({ data }: NodeProps) {
  const spec = data.spec as Extract<DslNode, { type: 'parallel' }>
  return (
    <NodeShell spec={spec}>
      <div className="bn-line">foreach {spec.parallel?.foreach ? refTail(spec.parallel.foreach) : '待填'}</div>
      <div className="bn-line bn-dim">
        body {TYPE_LABELS[spec.parallel?.body.type === 'tool' ? 'tool' : 'llm']} ·{' '}
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
