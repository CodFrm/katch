import { describe, expect, it } from 'vitest'

import {
  formatBytes,
  formatClock,
  formatCount,
  formatDuration,
  formatLatency,
  formatPercent,
  formatStamp,
  parseBytes,
  parseDuration,
} from '@/lib/format'

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

describe('formatCount', () => {
  // 不折成「万 / 亿」：那套进位只在中文里成立，翻成英文同一个数的量级都变了。
  const cases: [number, string][] = [
    [0, '0'],
    [-5, '0'],
    [999, '999'],
    [1240, '1,240'],
    [12400000, '12,400,000'],
  ]
  for (const [input, want] of cases) {
    it(`${input} 写成 ${want}`, () => {
      expect(formatCount(input)).toBe(want)
    })
  }
})

describe('formatStamp', () => {
  const now = new Date(2026, 8, 12, 14, 41)

  it('今天的事件只给时刻', () => {
    const at = new Date(2026, 8, 12, 9, 5)
    expect(formatStamp(at.getTime() / 1000, now)).toEqual({
      day: 'today',
      time: '09:05',
      date: '09-12',
    })
  })

  // 「昨天」按自然日算，不是按 24 小时：22:30 发生的事在第二天早上 8 点看
  // 已经是昨天了，哪怕还不到 24 小时。
  it('昨天按自然日判定', () => {
    const at = new Date(2026, 8, 11, 22, 30)
    expect(formatStamp(at.getTime() / 1000, now).day).toBe('yesterday')
  })

  it('再早的事件给日期', () => {
    const at = new Date(2026, 8, 9, 18, 4)
    expect(formatStamp(at.getTime() / 1000, now)).toEqual({
      day: 'earlier',
      time: '18:04',
      date: '09-09',
    })
  })
})

// 设置页把字节数与秒数写成人能改的短串（`3 TB`、`5m`），再解回机器要的数。
// 解析必须是 format 的逆：一个读得出来却写不回去的输入框，保存一次就把值改了。
describe('parseBytes', () => {
  const cases: [string, number | null][] = [
    ['3 TB', 3 * 1024 ** 4],
    ['3TB', 3 * 1024 ** 4],
    ['1.82 tb', Math.round(1.82 * 1024 ** 4)],
    ['512', 512],
    ['512 B', 512],
    ['', null],
    ['abc', null],
    ['-1 GB', null],
    ['3 PB?', null],
  ]
  for (const [input, want] of cases) {
    it(`${input || '空串'} -> ${want}`, () => {
      expect(parseBytes(input)).toBe(want)
    })
  }

  it('和 formatBytes 互为逆运算', () => {
    const bytes = 3 * 1024 ** 4
    const shown = formatBytes(bytes)
    expect(parseBytes(`${shown.value} ${shown.unit}`)).toBe(bytes)
  })
})

describe('formatDuration / parseDuration', () => {
  const cases: [number, string][] = [
    [30, '30s'],
    [300, '5m'],
    [3600, '1h'],
    [5400, '90m'],
    [86400, '24h'],
  ]
  for (const [seconds, text] of cases) {
    it(`${seconds} 秒写成 ${text}`, () => {
      expect(formatDuration(seconds)).toBe(text)
      expect(parseDuration(text)).toBe(seconds)
    })
  }

  it('不带单位按秒算，认不出来的给 null', () => {
    expect(parseDuration('45')).toBe(45)
    expect(parseDuration('5 分钟')).toBeNull()
    expect(parseDuration('')).toBeNull()
    expect(parseDuration('-5s')).toBeNull()
  })
})

describe('formatClock', () => {
  it('精确到秒，按本地时区', () => {
    const at = new Date(2025, 8, 12, 14, 32, 7)
    expect(formatClock(at.getTime() / 1000)).toBe('14:32:07')
  })

  it('个位数补零，读起来才对得齐', () => {
    const at = new Date(2025, 8, 12, 9, 5, 3)
    expect(formatClock(at.getTime() / 1000)).toBe('09:05:03')
  })
})

describe('formatLatency', () => {
  // 不到一秒的写毫秒：命中一份小文件是几毫秒的事，写成 0.0 s 就把「快得看不见」
  // 和「量到了零」说成了同一句话。
  const cases: [number, string][] = [
    [4, '4 ms'],
    [120, '120 ms'],
    [999, '999 ms'],
    [1000, '1.0 s'],
    [3200, '3.2 s'],
    [65400, '65.4 s'],
  ]
  for (const [ms, text] of cases) {
    it(`${ms} 毫秒写成 ${text}`, () => {
      expect(formatLatency(ms)).toBe(text)
    })
  }
})
