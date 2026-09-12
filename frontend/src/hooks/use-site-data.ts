import { useEffect, useState } from 'react'

import {
  fetchOverview,
  fetchUpstreams,
  fetchVersion,
  type Overview,
  type UpstreamItem,
  type VersionInfo,
} from '@/lib/api'

/** 侧栏那三个数看的区间。页脚那张表的命中率是后端按 24 小时算的，两者各问各的。 */
const OVERVIEW_RANGE = '30d'

export interface SiteData {
  /** 公开上游表；站长关掉公开首页时是 null，不是空数组——「没有」和「不给看」不是一回事。 */
  upstreams: UpstreamItem[] | null
  overview: Overview | null
  version: VersionInfo | null
  /** 三个请求都有结果了。在此之前不渲染识别结果，免得先给出一个没核对过的答案。 */
  loaded: boolean
}

/**
 * 取首页要用的三份公开数据。
 *
 * 任意一份取不到都不影响其余部分：是否公开统计与上游表是站长的设置，前端只
 * 渲染拿到的东西，拿不到的那块整块不出现。
 */
export function useSiteData(): SiteData {
  const [data, setData] = useState<Omit<SiteData, 'loaded'>>({
    upstreams: null,
    overview: null,
    version: null,
  })
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    const controller = new AbortController()
    void Promise.all([
      fetchUpstreams(controller.signal),
      fetchOverview(OVERVIEW_RANGE, controller.signal),
      fetchVersion(controller.signal),
    ]).then(([upstreams, overview, version]) => {
      if (controller.signal.aborted) {
        return
      }
      setData({ upstreams: upstreams?.list ?? null, overview, version })
      setLoaded(true)
    })
    return () => controller.abort()
  }, [])

  return { ...data, loaded }
}
