import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router'
import '@xyflow/react/dist/style.css'
import './styles/index.css'
import { AuthProvider, RequireAuth } from './auth.tsx'
import { LoginView } from './LoginView.tsx'
import { AppLayout } from './shell/AppLayout.tsx'
import { ExecutionsRoute } from './routes/ExecutionsRoute.tsx'
import { WorkflowsRoute } from './routes/WorkflowsRoute.tsx'

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
        { path: 'workflows', element: <WorkflowsRoute /> },
        { path: 'workflows/:draftId', element: <WorkflowsRoute /> },
        { path: 'executions', element: <ExecutionsRoute /> },
        { path: 'executions/:executionId', element: <ExecutionsRoute /> },
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
