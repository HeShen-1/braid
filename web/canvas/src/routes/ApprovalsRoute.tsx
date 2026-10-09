// 审批面路由:URL 是地址(/approvals 与 /approvals/:executionId/:nodeId)。
// 队列与详情同页;组件由 ApprovalsView 承载(T21.2 把原 /ui/ 原生三件套
// 并入同一 SPA 与同一设计系统,端点零新增)。
import { useNavigate, useParams } from 'react-router'
import { ApprovalsView } from '../ApprovalsView.tsx'
import { useAuth } from '../auth.tsx'

export function ApprovalsRoute() {
  const { executionId, nodeId } = useParams()
  const navigate = useNavigate()
  const { logout } = useAuth()

  const selected = executionId !== undefined && nodeId !== undefined ? { executionId, nodeId } : null

  return (
    <ApprovalsView
      selected={selected}
      onSelect={(sel) => void navigate(sel === null ? '/approvals' : `/approvals/${sel.executionId}/${sel.nodeId}`)}
      onAuthExpired={logout}
    />
  )
}
