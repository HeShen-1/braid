#!/usr/bin/env node
// 把 shadcn CLI 包内的 dist/tailwind.css 内联入库(D-2026-10-09-console-v2 决策 3)。
// 为什么不让 index.css 直接 @import "shadcn/tailwind.css":那会把 `shadcn` 钉进
// dependencies,而它带 33 个运行时依赖(babel / MCP SDK / execa / ora …)。
// 该文件只提供 data-* 自定义变体与 accordion 关键帧,是纯样式契约,内联后
// 运行时依赖面 = 0。升级组件时重跑本脚本,再 diff 入库文件。
import { readFileSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const src = require.resolve('shadcn/tailwind.css')
const body = readFileSync(src, 'utf8')
const header = `/* 由 scripts/vendor-shadcn-css.mjs 生成,请勿手改。
   来源:shadcn/dist/tailwind.css(来源版本见 package.json 的 devDependencies)。
   内容:data-* 自定义变体 + accordion 关键帧(组件样式契约)。
   重新生成:npm run vendor:shadcn-css */

`
writeFileSync('src/styles/shadcn-variants.css', header + body)
console.log('wrote src/styles/shadcn-variants.css', body.length, 'bytes')
