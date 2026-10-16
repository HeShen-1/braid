// 执行面路由:URL 是地址(/executions/:id),列表与详情同页。
// T21.1 Lane B 拥有本文件与 ExecutionsView / ExecutionDetail / ExecutionRibbon。
import { useNavigate, useParams } from 'react-router'
import { ExecutionsView } from '../ExecutionsView.tsx'
import { useAuth } from '../auth.tsx'

export function ExecutionsRoute() {
  const { executionId } = useParams()
  const navigate = useNavigate()
  const { logout, canAuthor } = useAuth()

  return (
    <ExecutionsView
      selectedId={executionId ?? null}
      onSelect={(id) => void navigate(id === null ? '/executions' : `/executions/${id}`)}
      onAuthExpired={logout}
      canManage={canAuthor}
    />
  )
}
