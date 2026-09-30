/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// 产物策略(D-2026-09-30-t141-canvas-pipeline #1):vite 构建直出
// internal/api/web/canvas/(入库,go:embed 单二进制);base 钉 /ui/canvas/
// ——与 web.go 的 /ui/*filepath 静态路由对齐,零后端新端点。
export default defineConfig({
  plugins: [react()],
  base: '/ui/canvas/',
  build: {
    outDir: '../../internal/api/web/canvas',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
})
