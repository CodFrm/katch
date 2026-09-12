import { describe, expect, it } from 'vitest'

import { formatBytes, formatPercent } from '@/lib/format'

describe('formatBytes', () => {
  const cases: [number, string, string][] = [
    [0, '0', 'B'],
    [512, '512', 'B'],
    [2048, '2.00', 'KB'],
    [224395264, '214', 'MB'],
    [871938031616, '812', 'GB'],
    [2001454279065, '1.82', 'TB'],
    [1023, '1023', 'B'],
    [1024, '1.00', 'KB'],
  ]
  for (const [bytes, value, unit] of cases) {
    it(`${bytes} 拆成 ${value} ${unit}`, () => {
      expect(formatBytes(bytes)).toEqual({ value, unit })
    })
  }

  // 字节数来自后端的 int64，前端不该因为一个负数或缺失值就整页白屏。
  it('负数按 0 处理', () => {
    expect(formatBytes(-1)).toEqual({ value: '0', unit: 'B' })
  })
})

describe('formatPercent', () => {
  it('比值按一位小数展开成百分数', () => {
    expect(formatPercent(0.914)).toBe('91.4')
  })

  it('0 和 1 不丢小数位', () => {
    expect(formatPercent(0)).toBe('0.0')
    expect(formatPercent(1)).toBe('100.0')
  })

  it('越界的比值被夹回 0~100', () => {
    expect(formatPercent(-0.2)).toBe('0.0')
    expect(formatPercent(1.5)).toBe('100.0')
  })
})
