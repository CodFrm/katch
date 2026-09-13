import { describe, expect, it } from 'vitest'

import type { DailyPoint, Overview, UpstreamSeriesPoint, UpstreamStatItem } from '@/lib/api'
import {
  aggregateUpstreams,
  dailyHitRates,
  errorRate,
  hitRate,
  miniBars,
  missReasonShares,
  originRequests,
  savedBytesToday,
  stackedBars,
  topOriginErrorUpstream,
} from '@/lib/stats'

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

function stat(patch: Partial<UpstreamStatItem>): UpstreamStatItem {
  return {
    upstream_id: 1,
    host: 'docker.io',
    requests: 0,
    hits: 0,
    denied: 0,
    origin_errors: 0,
    bytes_served: 0,
    bytes_origin: 0,
    degraded: false,
    retry_at: 0,
    ...patch,
  }
}

function point(patch: Partial<UpstreamSeriesPoint>): UpstreamSeriesPoint {
  return {
    bucket: 0,
    requests: 0,
    hits: 0,
    denied: 0,
    origin_errors: 0,
    bytes_served: 0,
    bytes_origin: 0,
    miss_first: 0,
    miss_ttl: 0,
    miss_evicted: 0,
    miss_changed: 0,
    ...patch,
  }
}

describe('originRequests 与 errorRate', () => {
  // 回源数和命中率同一个分母口径：被规则挡下、回源失败的请求都不算问过缓存。
  it('回源数是问过缓存又没命中的那些', () => {
    expect(originRequests(stat({ requests: 1000, hits: 942, denied: 8, origin_errors: 10 }))).toBe(
      40
    )
  })

  it('错误率的分母是总请求，一次都没来过时是 0', () => {
    expect(errorRate(stat({ requests: 1200, origin_errors: 20 }))).toBeCloseTo(0.0167, 4)
    expect(errorRate(stat({}))).toBe(0)
  })
})

describe('aggregateUpstreams', () => {
  it('把每个上游的计数加成全站合计', () => {
    const total = aggregateUpstreams([
      stat({
        requests: 1000,
        hits: 900,
        denied: 1,
        origin_errors: 2,
        bytes_served: 30,
        bytes_origin: 10,
      }),
      stat({
        requests: 200,
        hits: 100,
        denied: 3,
        origin_errors: 4,
        bytes_served: 8,
        bytes_origin: 5,
      }),
    ])
    expect(total).toEqual({
      requests: 1200,
      hits: 1000,
      denied: 4,
      origin_errors: 6,
      bytes_served: 38,
      bytes_origin: 15,
    })
  })

  it('一个上游都没有时给一份零，而不是 NaN', () => {
    expect(aggregateUpstreams([]).requests).toBe(0)
    expect(hitRate(aggregateUpstreams([]))).toBe(0)
  })
})

describe('topOriginErrorUpstream', () => {
  // 一个全站错误率不告诉任何人该去看哪台，而这份数据里已经有答案了。
  it('指出回源失败最多的那个上游', () => {
    const worst = topOriginErrorUpstream([
      stat({ host: 'docker.io', origin_errors: 2 }),
      stat({ host: 'pypi.org', origin_errors: 20 }),
    ])
    expect(worst?.host).toBe('pypi.org')
  })

  it('一个都没错时是 null，不硬挑一个出来', () => {
    expect(topOriginErrorUpstream([stat({}), stat({})])).toBeNull()
  })
})

describe('stackedBars', () => {
  // 高度按区间峰值归一，不是按每根柱子自己的量——那样每根都顶天，看不出忙闲。
  it('按区间峰值归一，回源段是这个小时里没命中的比例', () => {
    const bars = stackedBars([
      point({ bucket: 1, requests: 50, hits: 40 }),
      point({ bucket: 2, requests: 100, hits: 60 }),
    ])
    expect(bars[0]).toEqual({ bucket: 1, height: 0.5, originShare: 0.2 })
    expect(bars[1]).toEqual({ bucket: 2, height: 1, originShare: 0.4 })
  })

  it('被规则挡下与回源失败的请求不进这张图的分母', () => {
    const [bar] = stackedBars([
      point({ bucket: 1, requests: 100, hits: 40, denied: 50, origin_errors: 10 }),
    ])
    expect(bar.height).toBe(1)
    expect(bar.originShare).toBe(0)
  })

  it('一整段都没有请求时给一排零高度，而不是 NaN', () => {
    expect(stackedBars([point({ bucket: 1 }), point({ bucket: 2 })])).toEqual([
      { bucket: 1, height: 0, originShare: 0 },
      { bucket: 2, height: 0, originShare: 0 },
    ])
  })
})

describe('miniBars', () => {
  it('只取最后那几个小时，按这几个小时的峰值归一', () => {
    const points = [10, 20, 40].map((requests, index) => point({ bucket: index, requests }))
    expect(miniBars(points, 2)).toEqual([0.5, 1])
  })

  // 12 根柱子里有 3 根有数据，和 3 根柱子占满整块，说的是两件不同的事。
  it('数据不够时在前面补零，长度恒等于要的根数', () => {
    expect(miniBars([point({ requests: 5 })], 4)).toEqual([0, 0, 0, 1])
    expect(miniBars([], 3)).toEqual([0, 0, 0])
  })
})

describe('missReasonShares', () => {
  it('四个原因按固定顺序给出占比，加起来是 100', () => {
    const shares = missReasonShares([
      point({ miss_first: 40, miss_ttl: 14, miss_evicted: 5, miss_changed: 1 }),
      point({ miss_first: 22, miss_ttl: 7, miss_evicted: 6, miss_changed: 5 }),
    ])
    // 顺序是固定的，不按大小排：名次随刷新跳来跳去时，人会以为数据变了。
    expect(shares.map((s) => s.reason)).toEqual(['first', 'ttl', 'evicted', 'changed'])
    expect(shares.map((s) => s.count)).toEqual([62, 21, 11, 6])
    expect(shares.map((s) => s.percent)).toEqual([62, 21, 11, 6])
    expect(shares.reduce((sum, s) => sum + s.percent, 0)).toBe(100)
  })

  it('除不尽的时候也凑满 100，而不是 99', () => {
    // 逐项四舍五入会给出 33 + 33 + 33 = 99，图上就会缺一块，而缺的那块
    // 不写在任何一行上，看的人只会觉得这几个数算错了。
    const shares = missReasonShares([point({ miss_first: 1, miss_ttl: 1, miss_evicted: 1 })])
    expect(shares.map((s) => s.percent)).toEqual([34, 33, 33, 0])
    expect(shares.reduce((sum, s) => sum + s.percent, 0)).toBe(100)
  })

  it('一次回源都没有时给空，而不是四条零', () => {
    // 四条 0% 的条和「这段时间全是命中」长得一模一样，而后者是好消息。
    expect(missReasonShares([point({ requests: 10, hits: 10 })])).toEqual([])
    expect(missReasonShares([])).toEqual([])
  })

  it('未命中还没有归因的老数据也算没有数据', () => {
    // 补丁迁移之前落下的行四列都是 0，但这一小时确实有回源。按四项之和当分母，
    // 这种行给出的是「没有可分解的回源」，而不是一条 100% 的首次拉取。
    expect(missReasonShares([point({ requests: 10, hits: 4 })])).toEqual([])
  })
})
