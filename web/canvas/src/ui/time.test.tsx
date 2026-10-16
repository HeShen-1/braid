import { describe, expect, it } from 'vitest'
import { fmtAgo, fmtDuration } from './time.tsx'

describe('duration carry', () => {
  it.each([
    [999, '999ms'], [1499, '1.5s'], [59_499, '59s'], [59_500, '1m00s'],
    [479_499, '7m59s'], [479_500, '8m00s'], [3_599_500, '1h00m'],
    [86_399_500, '1d00h'], [-1, '—'], [NaN, '—'],
  ])('%d ms → %s', (ms, label) => expect(fmtDuration(ms)).toBe(label))

  it('相对时间沿用进位规则', () => {
    expect(fmtAgo('2026-10-16T00:00:00Z', Date.parse('2026-10-16T00:07:59.600Z'))).toBe('8m00s 前')
  })
})
