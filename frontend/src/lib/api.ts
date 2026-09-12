/**
 * 公开接口的响应形状与取数。
 *
 * 后端统一用 `{code, msg, data}` 包一层（cago 的 httputils.HandleResp），业务
 * 字段都在 `data` 里，所以这里剥掉那一层再交给界面，免得每个组件都记着它。
 */

/** 上游的协议类别，取值与后端 upstream_entity.Kind 一致。 */
export type UpstreamKind = 'registry' | 'static'

/** 上游此刻的可服务状态，只有这两种，取值与后端 api/upstream 的常量一致。 */
export type UpstreamStatus = 'normal' | 'degraded'

/** 公开上游列表里的一条。回源地址、默认策略这些运营字段不在公开接口里。 */
export interface UpstreamItem {
  host: string
  kind: UpstreamKind
  library_completion: boolean
  hit_rate: number
  cache_bytes: number
  status: UpstreamStatus
}

export interface UpstreamList {
  list: UpstreamItem[]
}

/** 近 14 天逐日序列里的一个自然日，后端已把缺的日子补成零点。 */
export interface DailyPoint {
  day: number
  requests: number
  hits: number
  bytes_served: number
  bytes_origin: number
}

export interface Overview {
  range: string
  from: number
  to: number
  requests: number
  hits: number
  denied: number
  origin_errors: number
  bytes_served: number
  bytes_origin: number
  cache_bytes: number
  daily: DailyPoint[]
}

export interface VersionInfo {
  version: string
  commit: string
}

interface Envelope<T> {
  code: number
  msg: string
  data: T
}

/**
 * 取一个公开接口，取不到就返回 null。
 *
 * 取不到有两种：站长把公开首页关了（匿名调用方收到 404），或者网络/后端出错。
 * 两者对首页是同一件事——这块内容这次没有，页面照常渲染其余部分。是否公开由
 * 后端决定，前端不替它解释，也不因此弹一条英文错误串。
 */
export async function getPublic<T>(path: string, signal?: AbortSignal): Promise<T | null> {
  try {
    const resp = await fetch(path, { signal, headers: { Accept: 'application/json' } })
    if (!resp.ok) {
      return null
    }
    const body = (await resp.json()) as Envelope<T>
    if (body.code !== 0) {
      return null
    }
    return body.data
  } catch {
    return null
  }
}

/** 统计区间，与后端 OverviewRequest.Range 的取值一致。 */
export type StatRange = '24h' | '7d' | '30d'

export function fetchOverview(range: StatRange, signal?: AbortSignal) {
  return getPublic<Overview>(`/api/v1/stats/overview?range=${range}`, signal)
}

export function fetchUpstreams(signal?: AbortSignal) {
  return getPublic<UpstreamList>('/api/v1/upstreams', signal)
}

export function fetchVersion(signal?: AbortSignal) {
  return getPublic<VersionInfo>('/api/v1/system/version', signal)
}
