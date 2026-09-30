// 测试共享:base fixtures 装载(非 .test 文件——被多个测试 import 时
// 不重复注册用例)。
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import type { WorkflowDoc } from './dsl.ts'

export function loadBase(name: string): WorkflowDoc {
  // jsdom 环境下 new URL(rel, import.meta.url) 解析失真,钉 dirname 拼 Path。
  return JSON.parse(
    readFileSync(resolve(import.meta.dirname, '../../testdata/base', `${name}.json`), 'utf8'),
  ) as WorkflowDoc
}
