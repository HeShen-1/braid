import { StrictMode, Suspense, lazy } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router'
import '@xyflow/react/dist/style.css'
import './styles/index.css'
import { AuthProvider, RequireAuth } from './auth.tsx'
import { LoginView } from './LoginView.tsx'
import { AppLayout } from './shell/AppLayout.tsx'
import { Skeleton } from './ui/primitives.tsx'
import { applyTheme, readTheme } from './theme.ts'

// 主题先于挂载落定:index.html 的启动脚本已做过一次(无闪烁),这里兜住
// 「启动脚本被策略拦掉 / 直接 import 本模块」的入口,幂等。
applyTheme(readTheme())

// 路由级代码分割:画布(xyflow)是最重的一块,不该让只看审批或执行面的人先下载它。
const WorkflowsRoute = lazy(() =>
  import('./routes/WorkflowsRoute.tsx').then((m) => ({ default: m.WorkflowsRoute })),
)
const ExecutionsRoute = lazy(() =>
  import('./routes/ExecutionsRoute.tsx').then((m) => ({ default: m.ExecutionsRoute })),
)
const ApprovalsRoute = lazy(() =>
  import('./routes/ApprovalsRoute.tsx').then((m) => ({ default: m.ApprovalsRoute })),
)

function RouteFallback() {
  return (
    <div className="flex flex-col gap-3 p-6" aria-busy="true" aria-label="加载中">
      <Skeleton width={240} height={16} />
      <Skeleton width={520} height={12} />
      <Skeleton width={420} height={12} />
    </div>
  )
}

// T21.2:路由硬切到根。vite base 已是 '/',basename 不再需要偏移;
// 三条深链(/workflows|/executions|/approvals 及各自子路径)由 web.go
// 的 SPA 回退承接,旧 /ui/* 一律 404。
const router = createBrowserRouter(
  [
    { path: '/login', element: <LoginView /> },
    {
      element: (
        <RequireAuth>
          <AppLayout />
        </RequireAuth>
      ),
      children: [
        { index: true, element: <Navigate to="/workflows" replace /> },
        {
          path: 'workflows',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <WorkflowsRoute />
            </Suspense>
          ),
        },
        {
          path: 'workflows/:draftId',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <WorkflowsRoute />
            </Suspense>
          ),
        },
        {
          path: 'executions',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <ExecutionsRoute />
            </Suspense>
          ),
        },
        {
          path: 'executions/:executionId',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <ExecutionsRoute />
            </Suspense>
          ),
        },
        {
          path: 'approvals',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <ApprovalsRoute />
            </Suspense>
          ),
        },
        {
          path: 'approvals/:executionId/:nodeId',
          element: (
            <Suspense fallback={<RouteFallback />}>
              <ApprovalsRoute />
            </Suspense>
          ),
        },
        { path: '*', element: <Navigate to="/workflows" replace /> },
      ],
    },
  ],
)

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider>
      <RouterProvider router={router} />
    </AuthProvider>
  </StrictMode>,
)
