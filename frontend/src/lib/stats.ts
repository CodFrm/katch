/**
 * 从公开总览的几个计数里推出侧栏要展示的三个数和那张 14 天趋势。
 *
 * 后端刻意不给命中率：它是几个计数的推导值，服务端多给一个数就多一处会和计数
 * 对不上的地方。推导规则写在这里，并被直接测。
 */

import type { Overview, UpstreamSeriesPoint, UpstreamStatItem } from './api'

/**
 * 一段区间里的四个计数。
 *
 * 全站总览与单上游统计都有这四个字段，命中率、回源数、错误率的算法对两者也完全
 * 一样，所以推导函数认的是这个形状而不是某一个接口的响应类型——同一个口径只该
 * 有一处实现。
 */
export interface TrafficCounters {
  requests: number
  hits: number
  denied: number
  origin_errors: number
}

/** 一段区间里的两个流量计数。 */
export interface TrafficBytes {
  bytes_served: number
  bytes_origin: number
}

/**
 * 命中率 = hits / (hits + misses)，其中 misses 由 requests 减去其余三项得到。
 *
 * 被规则挡下的和回源失败的请求都没问过缓存，把它们算进分母，一次上游故障就会
 * 显示成「缓存变差了」。没有任何问过缓存的请求时是 0。
 */
export function hitRate(counters: TrafficCounters): number {
  const asked = counters.requests - counters.denied - counters.origin_errors
  if (asked <= 0) {
    return 0
  }
  return Math.min(1, Math.max(0, counters.hits / asked))
}

/**
 * 回源请求数：问过缓存但没命中的那些。
 *
 * 和命中率同一个分母口径——被规则挡下的请求没问过缓存，也没去过上游。
 */
export function originRequests(counters: TrafficCounters): number {
  return Math.max(0, counters.requests - counters.denied - counters.origin_errors - counters.hits)
}

/**
 * 错误率 = 回源失败 / 总请求。
 *
 * 分母是总请求而不是问过缓存的那些：这个数回答的是「这台站现在有多少请求是坏
 * 的」，被规则挡下的请求也算在「来过」里。
 */
export function errorRate(counters: TrafficCounters): number {
  if (counters.requests <= 0) {
    return 0
  }
  return Math.min(1, Math.max(0, counters.origin_errors / counters.requests))
}

/** 省下的回源流量：发给客户端的字节减去真的从上游拉回来的字节。 */
export function savedBytes(bytes: TrafficBytes): number {
  return Math.max(0, bytes.bytes_served - bytes.bytes_origin)
}

/**
 * 把每个上游的计数加成全站合计。
 *
 * 全局指标条和健康矩阵读的是同一份按上游的统计，合计在前端算：再打一次总览接口
 * 只会让同一屏上的两个数来自两个时刻，然后对不上。
 */
export function aggregateUpstreams(list: UpstreamStatItem[]): TrafficCounters & TrafficBytes {
  return list.reduce<TrafficCounters & TrafficBytes>(
    (sum, item) => ({
      requests: sum.requests + item.requests,
      hits: sum.hits + item.hits,
      denied: sum.denied + item.denied,
      origin_errors: sum.origin_errors + item.origin_errors,
      bytes_served: sum.bytes_served + item.bytes_served,
      bytes_origin: sum.bytes_origin + item.bytes_origin,
    }),
    { requests: 0, hits: 0, denied: 0, origin_errors: 0, bytes_served: 0, bytes_origin: 0 }
  )
}

/**
 * 回源失败最多的那个上游，一个都没错时是 null。
 *
 * 错误率旁边那句「主要来自 …」就是它：一个全站错误率不告诉任何人该去看哪台，
 * 而这份数据里已经有答案了。
 */
export function topOriginErrorUpstream(list: UpstreamStatItem[]): UpstreamStatItem | null {
  let worst: UpstreamStatItem | null = null
  for (const item of list) {
    if (item.origin_errors > 0 && (!worst || item.origin_errors > worst.origin_errors)) {
      worst = item
    }
  }
  return worst
}

/** 一根堆叠柱：整根高度是相对峰值的比例，回源段占这根柱子的比例。 */
export interface StackedBar {
  bucket: number
  /** 0~1，相对于这段区间里最忙的那个小时。 */
  height: number
  /** 0~1，这个小时里回源请求占问过缓存的请求的比例。 */
  originShare: number
}

/**
 * 逐小时的命中/回源堆叠柱。
 *
 * 高度按区间峰值归一，而不是按每根柱子自己的量：那样画出来每根都顶天，看不出
 * 忙闲。全零的区间给一排零高度的柱子——后端已经把缺的小时补成零点了，那些空档
 * 本身就是信息，不该被丢掉。
 */
export function stackedBars(points: UpstreamSeriesPoint[]): StackedBar[] {
  const asked = points.map((point) =>
    Math.max(0, point.requests - point.denied - point.origin_errors)
  )
  const peak = Math.max(...asked, 0)
  return points.map((point, index) => {
    const total = asked[index]
    const origin = Math.max(0, total - point.hits)
    return {
      bucket: point.bucket,
      height: peak > 0 ? total / peak : 0,
      originShare: total > 0 ? Math.min(1, origin / total) : 0,
    }
  })
}

/**
 * 矩阵里那排迷你柱：最后 count 个小时的请求量，按这几个小时里的峰值归一。
 *
 * 不足 count 个小时时前面补零，长度恒为 count：12 根柱子里有 3 根有数据，和 3 根
 * 柱子占满整块，说的是两件不同的事。
 */
export function miniBars(points: UpstreamSeriesPoint[], count: number): number[] {
  const tail = points.slice(-count).map((point) => Math.max(0, point.requests))
  const padded = [...Array.from({ length: Math.max(0, count - tail.length) }, () => 0), ...tail]
  const peak = Math.max(...padded, 0)
  return padded.map((value) => (peak > 0 ? value / peak : 0))
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

/** 回源原因，顺序就是界面上从上到下的顺序。 */
export const MISS_REASONS = ['first', 'ttl', 'evicted', 'changed'] as const

export type MissReason = (typeof MISS_REASONS)[number]

/** 一个回源原因占这段区间里全部回源的比例。 */
export interface MissReasonShare {
  reason: MissReason
  /** 这个原因下的回源次数。 */
  count: number
  /** 占比，整数百分点，四项加起来恒为 100。 */
  percent: number
}

/**
 * 把逐小时序列里的四个原因加成一段区间的占比，一次回源都没有时给空数组。
 *
 * 分母是四项之和，不是「问过缓存又没命中」那个回源数：两者本该相等，但补丁
 * 迁移之前落下的行四列都是零，用后者当分母会画出一条既不是 100% 也没法解释的
 * 占比条。四项全零就是「这段时间没有可分解的回源」，界面据此不画东西——四条
 * 0% 的条和「全是命中」长得一模一样，而后者是好消息。
 *
 * 余数用最大余数法补给小数部分最大的那一项，而不是逐项四舍五入：后者会给出
 * 33+33+33=99，缺的那一个百分点不写在任何一行上，看的人只会觉得这几个数算错了。
 */
export function missReasonShares(points: UpstreamSeriesPoint[]): MissReasonShare[] {
  const counts = points.reduce<Record<MissReason, number>>(
    (sum, point) => ({
      first: sum.first + Math.max(0, point.miss_first),
      ttl: sum.ttl + Math.max(0, point.miss_ttl),
      evicted: sum.evicted + Math.max(0, point.miss_evicted),
      changed: sum.changed + Math.max(0, point.miss_changed),
    }),
    { first: 0, ttl: 0, evicted: 0, changed: 0 }
  )
  const total = MISS_REASONS.reduce((sum, reason) => sum + counts[reason], 0)
  if (total <= 0) {
    return []
  }
  const shares = MISS_REASONS.map((reason) => ({
    reason,
    count: counts[reason],
    percent: Math.floor((counts[reason] * 100) / total),
  }))
  // 平局按固定顺序（sort 是稳定的）：名次随刷新跳来跳去时，人会以为数据变了。
  const byRemainder = [...shares].sort(
    (a, b) =>
      (counts[b.reason] * 100) / total - b.percent - ((counts[a.reason] * 100) / total - a.percent)
  )
  let rest = 100 - shares.reduce((sum, share) => sum + share.percent, 0)
  for (const share of byRemainder) {
    if (rest <= 0) {
      break
    }
    share.percent += 1
    rest -= 1
  }
  return shares
}
