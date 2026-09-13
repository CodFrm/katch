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

// ── 管理接口 ──────────────────────────────────────────────────────────
//
// 后台那一面全部要密钥。密钥握在浏览器里（这一轮没有账号体系，只有一把密钥），
// 存在 localStorage 里让刷新不掉登录，退出时清掉。

/** 管理密钥在 localStorage 里的键名。退出登录删的就是它。 */
export const ADMIN_KEY_STORAGE = 'katch.admin-key'

/**
 * 管理请求失败的两种情形。
 *
 * 只分「密钥不认」和「够不到后端」两种，不再细分：后端对「密钥错了」与「没带
 * 密钥」刻意返回一模一样的 401，前端想分也分不出来，装作能分只会骗人。
 */
export type AdminFailure = 'unauthorized' | 'unreachable'

export type AdminResult<T> = { ok: true; data: T } | { ok: false; reason: AdminFailure }

/**
 * 带着管理密钥取一个接口。
 *
 * 失败时只回一个枚举，**不回后端的 msg**：那是一句英文，贴给用户等于把后端的
 * 内部话术当文案（「界面遵循」那一段）。要说什么由界面按自己的语言决定。
 */
async function adminGet<T>(
  path: string,
  key: string,
  signal?: AbortSignal
): Promise<AdminResult<T>> {
  try {
    const resp = await fetch(path, {
      signal,
      headers: { Accept: 'application/json', Authorization: `Bearer ${key}` },
    })
    if (resp.status === 401) {
      return { ok: false, reason: 'unauthorized' }
    }
    if (!resp.ok) {
      return { ok: false, reason: 'unreachable' }
    }
    const body = (await resp.json()) as Envelope<T>
    if (body.code !== 0) {
      return { ok: false, reason: 'unreachable' }
    }
    return { ok: true, data: body.data }
  } catch {
    return { ok: false, reason: 'unreachable' }
  }
}

/** 一条上游的登记信息，取值与后端 api/admin.UpstreamItem 一致。 */
export interface AdminUpstreamItem {
  id: number
  host: string
  kind: UpstreamKind
  origin: string
  enabled: boolean
  immutable_patterns: string[]
  mutable_ttl_seconds: number
  default_policy: string
  library_completion: boolean
  note: string
  createtime: number
  updatetime: number
}

/** 一个上游在一段区间里的表现。degraded 是此刻的内存态，不是区间统计。 */
export interface UpstreamStatItem {
  upstream_id: number
  host: string
  requests: number
  hits: number
  denied: number
  origin_errors: number
  bytes_served: number
  bytes_origin: number
  degraded: boolean
  retry_at: number
}

/** 一个小时桶里的量。桶起点是 UTC 秒，换算成本地时间是界面的事。 */
export interface UpstreamSeriesPoint {
  bucket: number
  requests: number
  hits: number
  denied: number
  origin_errors: number
  bytes_served: number
  bytes_origin: number
}

export interface UpstreamSeries {
  range: string
  from: number
  to: number
  bucket_seconds: number
  list: UpstreamSeriesPoint[]
}

/**
 * 时间线上的一条事件。
 *
 * kind 与 actor 是后端的稳定枚举，detail 是结构化的「改了什么」；三者都不是
 * 给人读的句子，界面按 kind 查翻译表再把 detail 的字段填进去。
 */
export interface EventItem {
  id: number
  kind: string
  actor: string
  upstream_id: number
  detail: Record<string, unknown>
  createtime: number
}

export function fetchAdminUpstreams(key: string, signal?: AbortSignal) {
  return adminGet<{ list: AdminUpstreamItem[] }>('/api/v1/admin/upstreams', key, signal)
}

export function fetchUpstreamStats(key: string, range: StatRange, signal?: AbortSignal) {
  return adminGet<{ list: UpstreamStatItem[] }>(
    `/api/v1/admin/stats/upstreams?range=${range}`,
    key,
    signal
  )
}

/** 单个上游的按小时时序。upstream_id 是必填的，后端不给「全部上游」的时序。 */
export function fetchUpstreamSeries(
  key: string,
  upstreamID: number,
  range: StatRange,
  signal?: AbortSignal
) {
  return adminGet<UpstreamSeries>(
    `/api/v1/admin/stats/upstreams/series?upstream_id=${upstreamID}&range=${range}`,
    key,
    signal
  )
}

export function fetchAdminEvents(key: string, limit: number, signal?: AbortSignal) {
  return adminGet<{ list: EventItem[] }>(`/api/v1/admin/events?limit=${limit}`, key, signal)
}

/**
 * 带密钥取站点总览。
 *
 * 总览本身是公开接口，但站长可以把公开首页关掉——那时匿名调用方拿 404，而带着
 * 密钥的调用方照样能读（后端的 allowPublicHome 就是这么写的）。后台读它只为拿
 * 全站缓存占用，所以这里一定要带上密钥。
 */
export function fetchAdminOverview(key: string, range: StatRange, signal?: AbortSignal) {
  return adminGet<Overview>(`/api/v1/stats/overview?range=${range}`, key, signal)
}
