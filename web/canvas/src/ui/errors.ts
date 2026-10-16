// 既有线协议的显示词表；不改变服务端错误码，未知值保留原码供对账。
import { ApiError, type ExecutionDetail, type NodeExecution } from '../api.ts'

export const ERROR_TEXT: Record<string, string> = {
  invalid_credentials: '用户名或密码错误。',
  user_disabled: '账号已停用。',
  too_many_attempts: '尝试次数过多，请稍后再试。',
  not_approver: '你不是本节点的审批人。',
  role_denied: '当前账号没有执行此操作的角色权限。',
  not_waiting: '该审批已收口，无需再操作。',
  not_approval: '目标节点不是审批节点。',
  already_decided: '已有审批人投出决定（或签首票定局）。',
  decision_conflict: '你已投过不同的决定，不可改判。',
  execution_terminal: '执行已终结，该审批不再生效。',
  bad_comment: '备注超过 2000 字上限。',
  not_found: '目标不存在或已不可用，请返回列表确认。',
  bad_id: '地址中的编号无效，请返回列表重新选择。',
  bad_request: '请求参数不完整或格式不正确，请检查输入。',
  invalid_dsl: '工作流配置校验未通过，请检查所列字段。',
  not_rollbackable: '当前执行状态不允许回滚或续跑，请刷新确认。',
  rollback_blocked_inflight: '仍有节点正在执行或等待审批；请等待其结束，或选择先停止未结束节点后再恢复。',
  rollback_too_large: '执行包含的节点行数超过恢复操作上限，请联系管理员检查容量配置。',
  bad_node_id: '回滚目标节点无效，请重新选择。',
  no_resume_target: '没有可续跑的失败、取消或待执行节点。',
  not_cancellable: '当前执行状态不允许取消，请刷新确认。',
  cancel_too_large: '执行包含的节点行数超过取消操作上限，请联系管理员检查容量配置。',
  conflict: '执行已被其他操作改变，请刷新后再操作。',
  internal: '服务暂时无法完成请求，请稍后重试。',
  network_error: '无法连接服务，请检查网络后重试。',
  definition_empty: '服务返回的定义内容缺失，请重试或联系管理员。',
  approval_rejected: '人工审批已驳回，执行按规则结束。',
  approval_timeout: '审批等待已超时，执行按配置结束。',
  node_failed: '节点执行失败；具体原因尚未确认，请查看原始错误与节点证据。',
  input_eval: '节点输入无法解析，请检查触发参数和上游输出。',
  branch_eval: '分支条件无法解析，请检查条件配置和输入。',
  llm_call_failed: '模型调用失败，请检查模型配置、服务连接及节点证据。',
  llm_rate_limited: '模型服务限制了请求频率，请稍后重试。',
}

export function errorCodeText(code: string): string {
  return Object.hasOwn(ERROR_TEXT, code) ? ERROR_TEXT[code]! : code
}

export function apiErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return ERROR_TEXT.network_error!
  const known = Object.hasOwn(ERROR_TEXT, err.code) ? ERROR_TEXT[err.code] : undefined
  if (known !== undefined) return `${known}（${err.code}）`
  if (err.status === 403) return `当前账号无权访问此资源（${err.code}）。`
  if (err.status === 404) return `目标不存在或已不可用，请返回列表确认（${err.code}）。`
  return `请求失败（${err.code}）。`
}

// scheduler 的 node_failed 是汇总代码。必须同时匹配当前节点行和确切的
// 执行错误前缀，才把某个节点的原因归于本次执行；不能凭历史驳回行猜测。
export function executionFailureNode(detail: ExecutionDetail): NodeExecution | undefined {
  if (detail.status !== 'FAILED' || detail.error_code !== 'node_failed') return undefined
  return detail.nodes.find((n) => n.status === 'FAILED' && n.error_code !== null
    && detail.error_message?.startsWith(`node ${n.node_id} failed: ${n.error_code}: `))
}

export function executionFailureText(detail: ExecutionDetail, node: NodeExecution | undefined): string {
  if (node?.error_code === 'llm_call_failed' && node.error_message === 'llm: request needs model and messages') {
    return '模型请求缺少模型名称或消息，请检查该节点的模型与提示词配置。'
  }
  return errorCodeText(node?.error_code ?? detail.error_code ?? '')
}
