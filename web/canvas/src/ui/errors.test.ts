import { describe, expect, it } from 'vitest'
import { ApiError, type ExecutionDetail } from '../api.ts'
import { apiErrorText, errorCodeText, executionFailureNode, executionFailureText } from './errors.ts'

describe('共享错误词表', () => {
  it('已知原因可读，未知原码与网络故障各自保留', () => {
    expect(apiErrorText(new ApiError(403, 'role_denied', []))).toContain('当前账号没有执行此操作的角色权限')
    expect(apiErrorText(new ApiError(500, 'future_protocol_code', []))).toContain('future_protocol_code')
    expect(errorCodeText('future_node_failure')).toBe('future_node_failure')
    expect(apiErrorText(new TypeError('offline'))).toContain('检查网络')
  })

  it('模型请求缺字段只按确切的当前节点证据解释，不能猜成网络失败', () => {
    const node = { node_id: 'llm', type: 'llm', status: 'FAILED', error_code: 'llm_call_failed', error_message: 'llm: request needs model and messages' }
    const detail = { status: 'FAILED', error_code: 'node_failed', error_message: 'node llm failed: llm_call_failed: llm: request needs model and messages', nodes: [node] } as ExecutionDetail
    expect(executionFailureText(detail, executionFailureNode(detail))).toContain('缺少模型名称或消息')
    const unknown = { ...detail, error_message: null }
    expect(executionFailureNode(unknown)).toBeUndefined()
    expect(executionFailureText(unknown, undefined)).toContain('具体原因尚未确认')
  })
})
