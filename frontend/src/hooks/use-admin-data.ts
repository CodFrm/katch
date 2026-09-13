import { useEffect, useRef, useState } from 'react'

import {
  fetchAdminEvents,
  fetchAdminOverview,
  fetchAdminUpstreams,
  fetchUpstreamSeries,
  fetchUpstreamStats,
  type AdminResult,
  type AdminUpstreamItem,
  type EventItem,
  type StatRange,
  type UpstreamSeriesPoint,
  type UpstreamStatItem,
} from '@/lib/api'

/** 事件流一次要多少条。概览上那块区域装得下的量级，翻页属于事后追查。 */
const EVENT_LIMIT = 30

/** 迷你柱看的是最近 12 小时，所以时序按 24 小时取，用尾部那一段。 */
const MINI_RANGE: StatRange = '24h'

/**
 * 把「密钥失效」这一种失败单独交回去。
 *
 * 只有 401 才踢出登录：够不到后端是这台机器的事，不是密钥的事，一次网络抖动把
 * 人踢回登录页并说「密钥不对」是在撒谎。
 */
function useRejectOnUnauthorized(onUnauthorized: () => void) {
  const ref = useRef(onUnauthorized)
  // 存在 ref 里而不是进依赖数组：回调的身份每次渲染都可能变，跟着它重取数据就会
  // 变成一个永不停的取数循环。
  useEffect(() => {
    ref.current = onUnauthorized
  }, [onUnauthorized])
  return ref
}

function unwrap<T>(result: AdminResult<T>, reject: () => void): T | null {
  if (result.ok) {
    return result.data
  }
  if (result.reason === 'unauthorized') {
    reject()
  }
  return null
}

export interface AdminUpstreamData {
  /** 上游的登记信息（类型、回源地址、启停），按 id 取用。 */
  upstreams: AdminUpstreamItem[]
  /** 每个上游在当前区间里的量，后端连一次请求都没有的上游也会给。 */
  stats: UpstreamStatItem[]
}

/**
 * 后台两个屏幕共用的那两份数据：上游登记信息与按上游的统计。
 *
 * 放在外壳上取一次，两个屏幕共用：概览的矩阵、侧栏的常驻列表和上游详情的指标条
 * 读的都是它，各取一次只会让同一屏上的数来自不同时刻。
 */
export function useAdminUpstreamData(
  key: string,
  range: StatRange,
  onUnauthorized: () => void
): AdminUpstreamData {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [data, setData] = useState<AdminUpstreamData>({ upstreams: [], stats: [] })

  useEffect(() => {
    const controller = new AbortController()
    void Promise.all([
      fetchAdminUpstreams(key, controller.signal),
      fetchUpstreamStats(key, range, controller.signal),
    ]).then(([upstreams, stats]) => {
      if (controller.signal.aborted) {
        return
      }
      const fire = () => reject.current()
      setData({
        upstreams: unwrap(upstreams, fire)?.list ?? [],
        stats: unwrap(stats, fire)?.list ?? [],
      })
    })
    return () => controller.abort()
  }, [key, range, reject])

  return data
}

export interface OverviewExtras {
  events: EventItem[]
  /** 全站缓存占用，读不到时是 null——「没有」和「没读到」不该长得一样。 */
  cacheBytes: number | null
  /** 每个上游最近若干小时的时序，按 upstream_id 取，矩阵里那排迷你柱用它。 */
  series: Record<number, UpstreamSeriesPoint[]>
}

/**
 * 概览独有的那几份数据：事件流、全站缓存占用，以及每个上游的迷你柱。
 *
 * 迷你柱只能按上游一个个问：时序接口的 upstream_id 是必填的（它给的是一个上游的
 * 逐小时序列）。等统计回来了再发这批请求，因为那份列表才是「有哪些上游」。
 */
export function useOverviewExtras(
  key: string,
  range: StatRange,
  stats: UpstreamStatItem[],
  onUnauthorized: () => void
): OverviewExtras {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [timeline, setTimeline] = useState<{ events: EventItem[]; cacheBytes: number | null }>({
    events: [],
    cacheBytes: null,
  })
  const [series, setSeries] = useState<Record<number, UpstreamSeriesPoint[]>>({})
  const ids = stats.map((item) => item.upstream_id).join(',')

  // 事件流与缓存占用跟上游列表无关，所以它们不跟着列表回来再取一遍。
  useEffect(() => {
    const controller = new AbortController()
    void Promise.all([
      fetchAdminEvents(key, EVENT_LIMIT, controller.signal),
      fetchAdminOverview(key, range, controller.signal),
    ]).then(([events, overview]) => {
      if (controller.signal.aborted) {
        return
      }
      const fire = () => reject.current()
      setTimeline({
        events: unwrap(events, fire)?.list ?? [],
        cacheBytes: unwrap(overview, fire)?.cache_bytes ?? null,
      })
    })
    return () => controller.abort()
  }, [key, range, reject])

  // 迷你柱只能按上游一个个问：时序接口的 upstream_id 是必填的。等列表回来了才知道
  // 要问谁，所以这一批挂在 ids 上。
  useEffect(() => {
    const upstreamIDs = ids ? ids.split(',').map(Number) : []
    if (upstreamIDs.length === 0) {
      return
    }
    const controller = new AbortController()
    void Promise.all(
      upstreamIDs.map((id) => fetchUpstreamSeries(key, id, MINI_RANGE, controller.signal))
    ).then((results) => {
      if (controller.signal.aborted) {
        return
      }
      const next: Record<number, UpstreamSeriesPoint[]> = {}
      results.forEach((result, index) => {
        const points = unwrap(result, () => reject.current())?.list
        if (points) {
          next[upstreamIDs[index]] = points
        }
      })
      setSeries(next)
    })
    return () => controller.abort()
  }, [key, ids, reject])

  return { ...timeline, series }
}

/**
 * 一个上游的逐小时时序，还没取到时是 null。
 *
 * null 与空数组要分开：空数组是「这 24 小时真的一次请求都没有」（后端会把缺的
 * 小时补成零点），null 是「还没问到」，图上该什么都不画。
 */
export function useUpstreamSeries(
  key: string,
  upstreamID: number,
  range: StatRange,
  onUnauthorized: () => void
): UpstreamSeriesPoint[] | null {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [loaded, setLoaded] = useState<{ id: number; points: UpstreamSeriesPoint[] } | null>(null)

  useEffect(() => {
    if (!Number.isFinite(upstreamID) || upstreamID <= 0) {
      return
    }
    const controller = new AbortController()
    void fetchUpstreamSeries(key, upstreamID, range, controller.signal).then((result) => {
      if (controller.signal.aborted) {
        return
      }
      const points = unwrap(result, () => reject.current())?.list
      if (points) {
        setLoaded({ id: upstreamID, points })
      }
    })
    return () => controller.abort()
  }, [key, upstreamID, range, reject])

  // 上一个上游的序列不许在这一个的图上出现：切换上游时手里那份数据属于别人，
  // 所以这里比对 id 而不是在 effect 里先把状态清空。
  return loaded && loaded.id === upstreamID ? loaded.points : null
}
