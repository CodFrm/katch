/**
 * 从公开总览的几个计数里推出侧栏要展示的三个数和那张 14 天趋势。
 *
 * 后端刻意不给命中率：它是几个计数的推导值，服务端多给一个数就多一处会和计数
 * 对不上的地方。推导规则写在这里，并被直接测。
 */

import type { Overview } from './api'

/**
 * 命中率 = hits / (hits + misses)，其中 misses 由 requests 减去其余三项得到。
 *
 * 被规则挡下的和回源失败的请求都没问过缓存，把它们算进分母，一次上游故障就会
 * 显示成「缓存变差了」。没有任何问过缓存的请求时是 0。
 */
export function hitRate(overview: Overview): number {
  const asked = overview.requests - overview.denied - overview.origin_errors
  if (asked <= 0) {
    return 0
  }
  return Math.min(1, Math.max(0, overview.hits / asked))
}

/** 最后一天省下的回源流量：发给客户端的字节减去真的从上游拉回来的字节。 */
export function savedBytesToday(overview: Overview): number {
  const today = overview.daily.at(-1)
  if (!today) {
    return 0
  }
  return Math.max(0, today.bytes_served - today.bytes_origin)
}

export interface DailyHitRate {
  day: number
  rate: number
}

/**
 * 逐日命中率。
 *
 * 逐日序列里没有 denied / origin_errors，分母只能是 requests——它和上面那个
 * 区间命中率口径不同，所以这张图只用来看趋势，绝对值以侧栏那个数为准。
 * 零请求的日子留成 0 而不是被丢掉：后端已经把缺的日子补齐了，图上那些空档
 * 本身就是「这几天没人来」的信息。
 */
export function dailyHitRates(overview: Overview): DailyHitRate[] {
  return overview.daily.map((point) => ({
    day: point.day,
    rate: point.requests > 0 ? Math.min(1, Math.max(0, point.hits / point.requests)) : 0,
  }))
}
