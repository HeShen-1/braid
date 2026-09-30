// palette 新建节点的默认 spec:字段形状对齐 schema,值留「显式待填」
// (空串/占位),发布校验的错误经保存回显引导作者补全——画布不做
// 前端复刻校验(权威在服务端,D-t141 #2)。
import type { DslNode, NodeType, WorkflowDoc } from './model/dsl.ts'

export const TYPE_LABELS: Record<NodeType, string> = {
  llm: 'LLM',
  tool: '工具',
  branch: '分支',
  approval: '审批',
  parallel: '并行',
}

export function blankDoc(): WorkflowDoc {
  return { dsl: '1.0', name: 'untitled-workflow', edges: [], nodes: [] }
}

export function defaultSpec(type: NodeType, id: string): DslNode {
  switch (type) {
    case 'llm':
      return { id, type, title: '', prompt: { template: '' }, outputs: {} }
    case 'tool':
      return { id, type, title: '', tool: '', inputs: {}, outputs: {} }
    case 'branch':
      return {
        id,
        type,
        title: '',
        cases: [{ when: { left: '', op: 'eq', right: '' }, next: '' }],
        default: '',
      }
    case 'approval':
      return {
        id,
        type,
        title: '',
        approvers: [],
        mode: 'any', // 现词域;t14.2 合入后加 "all"(D-t141 #5)
        timeout_hours: 72,
        on_timeout: 'escalate',
        instructions: '',
        outputs: { decision: 'string', comment: 'string' },
      }
    case 'parallel':
      return {
        id,
        type,
        title: '',
        parallel: {
          foreach: '',
          // on_failure 缺省 = fail_fast(引擎物化);置 '' 会违 schema 词域
          // enum ["fail_fast","wait_all"](对抗审查 HIGH),序列化时丢弃。
          on_failure: undefined,
          body: { type: 'llm', prompt: { template: '' }, outputs: {} },
        },
      }
  }
}
