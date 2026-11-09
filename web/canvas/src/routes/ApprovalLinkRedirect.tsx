// 已发送的审批通知沿用旧地址；统一转到当前审批详情路由。
import { Navigate, useLocation, useParams } from 'react-router'

export function ApprovalLinkRedirect() {
  const { executionId, nodeId } = useParams()
  const { search, hash } = useLocation()
  return <Navigate replace to={{
    pathname: `/approvals/${encodeURIComponent(executionId ?? '')}/${encodeURIComponent(nodeId ?? '')}`,
    search,
    hash,
  }} />
}
