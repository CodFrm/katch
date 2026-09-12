import { describe, expect, it } from 'vitest'

import type { DailyPoint, Overview } from '@/lib/api'
import { dailyHitRates, hitRate, savedBytesToday } from '@/lib/stats'

function overview(patch: Partial<Overview>): Overview {
  return {
    range: '30d',
    from: 0,
    to: 0,
    requests: 0,
    hits: 0,
    denied: 0,
    origin_errors: 0,
    bytes_served: 0,
    bytes_origin: 0,
    cache_bytes: 0,
    daily: [],
    ...patch,
  }
}

function day(patch: Partial<DailyPoint>): DailyPoint {
  return { day: 0, requests: 0, hits: 0, bytes_served: 0, bytes_origin: 0, ...patch }
}

describe('hitRate', () => {
  // 后端不给命中率，只给几个计数：它是 hits/(hits+misses)，而未命中数是
  // requests 减去 hits、denied、origin_errors。被规则挡下和回源失败的请求
  // 都没问过缓存，算进分母会让一次上游故障看起来像缓存变差了。
  it('被拒绝与回源失败的请求不进分母', () => {
    expect(
      hitRate(overview({ requests: 200, hits: 90, denied: 50, origin_errors: 50 }))
    ).toBeCloseTo(0.9)
  })

  it('全部命中是 1', () => {
    expect(hitRate(overview({ requests: 10, hits: 10 }))).toBe(1)
  })

  it('没有任何问过缓存的请求时是 0，而不是除零', () => {
    expect(hitRate(overview({ requests: 10, hits: 0, denied: 10 }))).toBe(0)
    expect(hitRate(overview({}))).toBe(0)
  })
})

describe('savedBytesToday', () => {
  // 省下的回源流量 = 发给客户端的字节 - 真的从上游拉回来的字节。
  it('取最后一天的服务字节减去回源字节', () => {
    const o = overview({
      daily: [
        day({ day: 1, bytes_served: 999, bytes_origin: 0 }),
        day({ day: 2, bytes_served: 500, bytes_origin: 160 }),
      ],
    })
    expect(savedBytesToday(o)).toBe(340)
  })

  it('回源比服务还多时按 0 算，不给出负数', () => {
    const o = overview({ daily: [day({ bytes_served: 10, bytes_origin: 40 })] })
    expect(savedBytesToday(o)).toBe(0)
  })

  it('序列为空时是 0', () => {
    expect(savedBytesToday(overview({}))).toBe(0)
  })
})

describe('dailyHitRates', () => {
  it('逐日给出命中率，零请求的日子是 0 而不是被丢掉', () => {
    const o = overview({
      daily: [
        day({ day: 1, requests: 0, hits: 0 }),
        day({ day: 2, requests: 4, hits: 1 }),
        day({ day: 3, requests: 2, hits: 2 }),
      ],
    })
    expect(dailyHitRates(o)).toEqual([
      { day: 1, rate: 0 },
      { day: 2, rate: 0.25 },
      { day: 3, rate: 1 },
    ])
  })
})
