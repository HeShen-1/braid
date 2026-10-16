import type { ExecutionSummary } from './api.ts'

export const OVERVIEW_SAMPLE_LIMIT = 100

export interface CreationBucket { start: number; end: number; count: number }

/** 只聚合本次响应的 created_at；轮询之间不累积，不推导状态历史。 */
export function creationDistribution(items: Pick<ExecutionSummary, 'created_at'>[]) {
  const times = items.map(({ created_at }) => typeof created_at === 'string' && created_at.trim() !== '' ? Date.parse(created_at) : NaN).filter(Number.isFinite)
  const earliest = times.length > 0 ? Math.min(...times) : null
  const latest = times.length > 0 ? Math.max(...times) : null
  const result = { sampleCount: items.length, invalidCount: items.length - times.length, earliest, latest, bucketMs: 0, buckets: [] as CreationBucket[] }
  if (earliest === null || latest === null || earliest === latest) return result
  // 对齐 UTC epoch 的等宽桶，选自然单位，最多 13 桶；末端边界归下一桶。
  const widths = [1, 10, 100, 1000, 10_000, 60_000, 300_000, 900_000, 3_600_000, 10_800_000, 86_400_000, 604_800_000, 2_592_000_000, 31_536_000_000]
  const target = (latest - earliest) / 12
  const bucketMs = widths.find((width) => width >= target) ?? Math.ceil(target / 31_536_000_000) * 31_536_000_000
  const start = Math.floor(earliest / bucketMs) * bucketMs
  const buckets = Array.from({ length: Math.floor((latest - start) / bucketMs) + 1 }, (_, index) => ({ start: start + index * bucketMs, end: start + (index + 1) * bucketMs, count: 0 }))
  for (const time of times) buckets[Math.floor((time - start) / bucketMs)].count++
  return { ...result, bucketMs, buckets }
}

export function bucketWidthText(ms: number) {
  for (const [unit, size] of [['天', 86_400_000], ['小时', 3_600_000], ['分钟', 60_000], ['秒', 1000], ['毫秒', 1]] as const) {
    if (ms >= size && ms % size === 0) return `${ms / size}${unit}`
  }
  return `${ms}毫秒`
}

/** 显式 UTC，保留毫秒，样本范围和可查桶边界使用同一口径。 */
export function overviewTime(ms: number) { return new Date(ms).toISOString().replace('T', ' ').replace('Z', '') }
