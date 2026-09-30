import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// vitest 未开 globals:true → RTL 不自动 cleanup,同文件用例 DOM 累积
// 会把「唯一元素」断言炸成 multiple;显式挂。
afterEach(cleanup)

// React Flow(v12)在 jsdom 下的最小宿主垫片:RF 依赖 ResizeObserver
// 观测容器,d3-zoom 读 DOMMatrix;真实浏览器都有,jsdom 都没有。
class ResizeObserverMock {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = ResizeObserverMock as unknown as typeof ResizeObserver
}
if (typeof (globalThis as Record<string, unknown>).DOMMatrixReadOnly === 'undefined') {
  class DOMMatrixReadOnlyMock {
    m22: number
    constructor(transform?: string) {
      const scale = transform?.match(/scale\(([1-9.]+)\)/)?.[1]
      this.m22 = scale !== undefined ? Number(scale) : 1
    }
  }
  ;(globalThis as Record<string, unknown>).DOMMatrixReadOnly = DOMMatrixReadOnlyMock
}
