import { describe, expect, it } from 'vitest'
import { bucketWidthText, creationDistribution, overviewTime } from './executionOverview.ts'

const created = (ms: number) => ({ created_at: new Date(ms).toISOString() })

describe('执行创建分布', () => {
  it('空样本、缺失时间、单个时间点都不伪造图形', () => {
    expect(creationDistribution([])).toMatchObject({ sampleCount: 0, invalidCount: 0, buckets: [] })
    expect(creationDistribution([{ created_at: '' }, { created_at: 'invalid' }])).toMatchObject({ sampleCount: 2, invalidCount: 2, earliest: null, latest: null, buckets: [] })
    expect(creationDistribution([created(1000), created(1000)])).toMatchObject({ sampleCount: 2, invalidCount: 0, earliest: 1000, latest: 1000, buckets: [] })
  })
  it('按真实时间等宽分桶，边界事件恰好归一次，缺值不变成 epoch 或零', () => {
    const result = creationDistribution([created(120_000), created(0), created(60_000), created(60_000), { created_at: '' }])
    expect(result).toMatchObject({ sampleCount: 5, invalidCount: 1, earliest: 0, latest: 120_000, bucketMs: 10_000 })
    expect(result.buckets).toHaveLength(13)
    expect(result.buckets[0].count).toBe(1)
    expect(result.buckets[6].count).toBe(2)
    expect(result.buckets[12].count).toBe(1)
    expect(result.buckets.reduce((sum, bucket) => sum + bucket.count, 0)).toBe(4)
    expect(result.buckets.every((bucket) => bucket.end - bucket.start === result.bucketMs)).toBe(true)
  })
  it('不依赖响应顺序或本机时区，毫秒和跨年范围仍保留真实计数', () => {
    const unordered = [created(Date.UTC(2026, 0, 1)), created(Date.UTC(2020, 0, 1)), { created_at: '2026-01-01T08:00:00+08:00' }]
    const result = creationDistribution(unordered)
    expect(result.buckets.length).toBeLessThanOrEqual(13)
    expect(result.buckets.reduce((sum, bucket) => sum + bucket.count, 0)).toBe(3)
    expect(creationDistribution([created(1), created(2)]).buckets.map((bucket) => bucket.count)).toEqual([1, 1])
    expect(overviewTime(1)).toBe('1970-01-01 00:00:00.001')
    expect(bucketWidthText(10_000)).toBe('10秒')
    expect(bucketWidthText(300_000)).toBe('5分钟')
    expect(bucketWidthText(86_400_000)).toBe('1天')
  })
})
