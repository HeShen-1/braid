#!/usr/bin/env node
// 守卫:`shadcn add <组件>` 会把该组件在 registry 里的**全部依赖**一并拷进来,
// 而 base-nova registry 的每个组件都依赖 button —— 于是每次 add 都会重新
// 生成 src/components/ui/button.tsx,与自研原语 web/canvas/src/ui/primitives.tsx
// 的 Button 撞名(两套按钮语言,正是 T21.3 要消灭的东西)。
//
// 本脚本在 add 之后强制收口:出现禁止项即失败,并打印收口命令。
// 用法:npm run check:shadcn  (已接进 test 前置,见 package.json)
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'

const UI = 'src/components/ui'

// 禁止项 = shadcn registry 里与自研原语/单点收敛冲突的文件
const FORBIDDEN = {
  'button.tsx': '与自研 Button 原语(ui/primitives.tsx)重复;T21.3 决策 3 规定基础原语保留自研',
}

// 允许项 = T21.3 决策 3 批准的 8 个复合交互组件
const ALLOWED = new Set([
  'dialog.tsx',
  'select.tsx',
  'dropdown-menu.tsx',
  'tabs.tsx',
  'tooltip.tsx',
  'popover.tsx',
  'checkbox.tsx',
  'switch.tsx',
])

const problems = []

if (existsSync(UI)) {
  for (const f of readdirSync(UI)) {
    if (FORBIDDEN[f]) problems.push(`src/components/ui/${f} —— ${FORBIDDEN[f]}`)
    else if (!ALLOWED.has(f)) problems.push(`src/components/ui/${f} —— 未在决策 3 批准清单内`)
  }
}
// cn 包装是 init 生成的中间层,我们直接消费 `cn` 包
const utils = 'src/lib/utils.ts'
if (existsSync(utils) && /export\s*\{\s*cn\s*\}\s*from\s*["']cn["']/.test(readFileSync(utils, 'utf8')) === false) {
  if (/\bconst\s+cn\b|function\s+cn\b/.test(readFileSync(utils, 'utf8'))) {
    problems.push(`${utils} —— 本地实现 cn;应直接 re-export \`cn\` 包(单点收敛)`)
  }
}

if (problems.length > 0) {
  console.error('shadcn add 收口检查失败:')
  for (const p of problems) console.error('  -', p)
  console.error('\n收口:rm <上述文件>,并确认所需组件已在决策 3 的 8 组件清单内。')
  process.exit(1)
}
console.log('shadcn 组件目录符合决策 3 清单')
