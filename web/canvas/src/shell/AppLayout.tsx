// 路由布局:壳 + 工作流工作区状态 + 出口。
// 工作区状态必须挂在出口之上——切到执行面再回来,未保存的编辑还在。
import { Outlet } from 'react-router'
import { AppShell } from './AppShell.tsx'
import { WorkflowProvider } from '../workflow/WorkflowProvider.tsx'

export function AppLayout() {
  return (
    <AppShell>
      <WorkflowProvider>
        <Outlet />
      </WorkflowProvider>
    </AppShell>
  )
}
