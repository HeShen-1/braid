/// <reference types="vitest/config" />
import { fileURLToPath } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// 产物策略(D-2026-09-30-t141-canvas-pipeline #1):vite 构建直出
// internal/api/web/(入库,go:embed 单二进制)。
// T21.1(D-2026-10-09-ui-console):Tailwind v4 CSS-first(@theme 写 token,
// 无 tailwind.config.js)。
// T21.2(同条 D-,决策 6):路由硬切到根 —— base 改 '/',outDir 上提到
// internal/api/web(审批台三件套随之退役,同目录清理由 emptyOutDir 承担);
// /assets/* 与 /workflows|/executions|/approvals 三条深链由 web.go 承接。
// T21.3(D-2026-10-09-console-v2 决策 4):`@/*` → src/* 别名(shadcn CLI
// 前置条件,tsconfig.paths 同源)。既有相对 import 不动,纯增量。
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  base: '/',
  build: {
    outDir: '../../internal/api/web',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
})
