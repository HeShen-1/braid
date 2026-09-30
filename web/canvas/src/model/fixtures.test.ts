// 导出 fixtures 契约:base(画布输入)→ toCanvas → fromCanvas →
// serializeDoc 的产物 = testdata/exports/<name>.json(逐字节)。
// exports 同时是 Go 侧跨语言契约测试的输入(internal/api 读同批文件
// 走 dsl.Parse + 发布同口径校验,D-t141 #2)——本文件更新 fixtures 后
// 必须同 commit 提交两侧,漂移在 CI web job 与 go test 双面红。
// 重新生成:UPDATE_FIXTURES=1 npm test(web/canvas 下)。

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { fromCanvas, serializeDoc, toCanvas } from './convert.ts'
import { loadBase } from './testdata.ts'

const EXPORTS_DIR = resolve(import.meta.dirname, '../../testdata/exports')

const BASES = ['all-node-types', 'with-positions', 'minimal-llm']

describe('导出 fixtures(跨语言契约面)', () => {
  for (const name of BASES) {
    it(`${name}:导出字节 == exports/${name}.json`, () => {
      const base = loadBase(name)
      const out = serializeDoc(fromCanvas(toCanvas(base), base))
      const file = resolve(EXPORTS_DIR, `${name}.json`)
      if (process.env.UPDATE_FIXTURES === '1') {
        mkdirSync(dirname(file), { recursive: true })
        writeFileSync(file, out)
        return
      }
      expect(out).toBe(readFileSync(file, 'utf8'))
    })
  }

  it('导出幂等:导出产物再走一遍转换,字节不变(canonical 稳定)', () => {
    const base = loadBase('with-positions')
    const once = serializeDoc(fromCanvas(toCanvas(base), base))
    const twice = serializeDoc(fromCanvas(toCanvas(JSON.parse(once)), JSON.parse(once)))
    expect(twice).toBe(once)
  })
})
