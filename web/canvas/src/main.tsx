import { StrictMode, Suspense, lazy } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router'
import '@xyflow/react/dist/style.css'
import './styles/index.css'
import { AuthProvider, RequireAuth } from './auth.tsx'
import { LoginView } from './LoginView.tsx'
import { AppLayout } from './shell/AppLayout.tsx'
import { Skeleton } from './ui/primitives.tsx'

// 路由级代码分割:画布(xyflow)是最重的一块,不该让只看执行面的人先下载它。
const WorkflowsRoute = lazy(() =>
  import('./routes/WorkflowsRoute.tsx').then((m) => ({ default: m.WorkflowsRoute })),
)
const ExecutionsRoute = lazy(() =>
  import('./routes/ExecutionsRoute.tsx').then((m) => ({ default: m.ExecutionsRoute })),
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

// basename 取自 vite base:PR1 = /ui/canvas,PR2 硬切到根只改一处 base。
// 路由路径本身从 PR1 起就是最终形态(/workflows、/executions)。
const base = import.meta.env.BASE_URL.replace(/\/$/, '')

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
        { path: '*', element: <Navigate to="/workflows" replace /> },
      ],
    },
  ],
  { basename: base === '' ? '/' : base },
)

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider>
      <RouterProvider router={router} />
    </AuthProvider>
  </StrictMode>,
)
