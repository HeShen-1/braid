import { defineConfig } from '@playwright/test'

// Playwright e2e(D-2026-10-04-t166-playwright-e2e):
// - testDir = e2e/,spec 命名 *.e2e.ts——vitest include 钉 src/**/*.test.*
//   (D-t141 决策 2),两侧天然不相交,不引入第二测试根目录。
// - 栈由 deploy/e2e-up.sh(CI = job services + 宿主进程)先行起好,config
//   不配 webServer——被测面是 Go api 内嵌的入库 dist,不是 npm dev server。
// - 串行:全部 spec 共享单执行栈 + mock http fixture 端口,并行必互踩。
// - retries = 0:断言面是流程正确性,重试掩盖 flaky = 假绿(疑 flakes 按
//   仓纪律 rerun + 对拍,不在 harness 内自动重试)。
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.e2e.ts',
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: process.env.BRAID_E2E_BASE_URL ?? 'http://127.0.0.1:18081',
    trace: 'retain-on-failure',
  },
})
