// DSL 文档的 TypeScript 镜像(internal/engine/dsl/schema.json 契约)。
// 画布消费的权威校验在服务端(schema + 发布白名单);本镜像只为
// 双向转换提供类型面,不复制校验逻辑。
// 关键形状:llm/tool/branch/approval 专有字段与公共字段平铺,parallel
// 专有字段嵌套在 "parallel" 键下(engine/dsl/types.go 两遍解码同构)。

export const NODE_TYPES = ['llm', 'tool', 'branch', 'approval', 'parallel'] as const
export type NodeType = (typeof NODE_TYPES)[number]

// schema branchNode.when.op 枚举的镜像(8 值):面板与测试共同消费,
// schema 改枚举时此单点跟改(对抗审查:镜像漂移让 contains 在面板失踪)。
export const BRANCH_OPS = ['eq', 'ne', 'gt', 'gte', 'lt', 'lte', 'contains', 'exists'] as const
export type BranchOp = (typeof BRANCH_OPS)[number]

// schema $defs/identifier:^[a-z][a-z0-9_]{0,63}$
export const ID_PATTERN = /^[a-z][a-z0-9_]{0,63}$/

// schema $defs/sourceAnchor:^(case_[0-9]{1,3}|default)$ —— 节点源锚点词域
// (T21.4 ③ 多锚点连线)。词域真源在 Schema(画布导出 fixtures 经 dsl.Parse
// 校验),此处是跨语言镜像:推导出的锚点 id 必须先过本正则才可能落盘,
// 否则导出会撞发布校验。两侧改动必须同 commit(schema_test 有反锚样例)。
export const SOURCE_ANCHOR_PATTERN = /^(case_[0-9]{1,3}|default)$/

/** branch 第 i 条 case 的源锚点 id(下标对齐 cases 数组序)。 */
export function caseAnchorId(index: number): string {
  return `case_${index}`
}

/** branch 默认去向的源锚点 id。 */
export const DEFAULT_SOURCE_ANCHOR = 'default'

/** schema metadata.canvas.edges 元素:边 → 源锚点绑定(引擎忽略)。 */
export interface CanvasEdgeAnchor {
  from: string
  to: string
  /** 缺省 = 未绑定,该边落 from 节点的缺省手柄。 */
  source_anchor?: string
}

/** schema $defs/point:画布坐标(取整后落盘)。 */
export interface Point {
  x: number
  y: number
}

export interface Viewport {
  x: number
  y: number
  zoom: number
}

/** schema metadata.canvas:节点位置 + 视口(zoom 0.1..4)+ 连线锚点绑定。引擎忽略、原文字节保留。 */
export interface CanvasMeta {
  nodes: Record<string, { position: Point }>
  viewport?: Viewport
  edges?: CanvasEdgeAnchor[]
}

export interface WorkflowMeta {
  canvas?: CanvasMeta
}

export interface DslInput {
  name: string
  type: string // string|number|boolean|object|array
  required?: boolean
  description?: string
}

export interface DslEdge {
  from: string
  to: string
  label?: string
}

export interface DslRetry {
  max_attempts: number
}

export interface DslPrompt {
  system?: string
  template: string
  user_data?: DslUserData[]
}

export interface DslUserData {
  name: string
  ref: string
}

export interface DslNodeCommon {
  id: string
  type: NodeType
  title?: string
  description?: string
  retry?: DslRetry
}

export interface LlmFields {
  model?: string
  temperature?: number
  max_tokens?: number
  response_format?: string
  prompt: DslPrompt
  outputs?: Record<string, string>
}

export interface ToolFields {
  tool: string
  inputs: Record<string, unknown> // schema required(palette 默认恒带 {})
  outputs?: Record<string, string>
}

export interface BranchCase {
  when: { left: string; op: string; right?: unknown }
  next: string
}

export interface BranchFields {
  cases: BranchCase[]
  default: string
}

export interface ApprovalFields {
  approvers: string[] // t14.2 起 schema 强制 uniqueItems
  mode?: string // 词域 ["any","all"](t14.2 会签已合入,D-t141 #5 已消费)
  timeout_hours?: number
  on_timeout?: string
  instructions?: string
  outputs?: Record<string, string>
}

/** T9.1 Scatter-Gather:foreach 纯引用;body 是 llm|tool 模板(ID 恒空缺席)。 */
export interface LlmNode extends DslNodeCommon, LlmFields {
  type: 'llm'
}

export interface ToolNode extends DslNodeCommon, ToolFields {
  type: 'tool'
}

export interface BranchNode extends DslNodeCommon, BranchFields {
  type: 'branch'
}

export interface ApprovalNode extends DslNodeCommon, ApprovalFields {
  type: 'approval'
}

export interface ParallelNode extends DslNodeCommon {
  type: 'parallel'
  parallel: DslParallel
}

export type DslNode = LlmNode | ToolNode | BranchNode | ApprovalNode | ParallelNode

type DistOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never

/** parallel body:llm|tool 单节点模板,id 缺席(ID 恒空,实例共享父 node_id)。 */
export type ParallelBody = DistOmit<LlmNode | ToolNode, 'id'>

export interface DslParallel {
  foreach: string
  on_failure?: string // 空 = "fail_fast"
  body: ParallelBody
}

export interface WorkflowDoc {
  dsl: string
  name: string
  // $comment 是 schema 合法顶层字段(文档注释):画布透传保真,
  // 丢弃即静默数据丢失(对抗审查补验 MEDIUM)。
  $comment?: string
  description?: string
  metadata?: WorkflowMeta
  inputs?: DslInput[]
  edges?: DslEdge[] // schema 顶层 required 仅 [dsl, name, nodes]
  nodes: DslNode[]
  outputs?: Record<string, string>
}

export function isNodeType(x: unknown): x is NodeType {
  return typeof x === 'string' && (NODE_TYPES as readonly string[]).includes(x)
}
