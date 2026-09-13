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
  /** 从没缓存过。 */
  miss_first: number
  /** 可变对象的 TTL 过期了。 */
  miss_ttl: number
  /** 不可变对象被淘汰了。 */
  miss_evicted: number
  /** 上游的 digest 和我们手上那份对不上。 */
  miss_changed: number
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

/**
 * 一次拉取在结构化日志里留下的那一行，取值与后端 api/admin.RecentRequest 一致。
 *
 * 它不来自数据库：每请求写库会把拉取热路径拖进事务（决策 16），这几个字段本来
 * 就是日志已经记着的东西。`result` 与 katch_requests_total 的 result 同一套取值。
 */
export interface RecentRequestItem {
  at: number
  object: string
  result: 'hit' | 'miss' | 'denied' | 'origin_error'
  bytes: number
  duration_ms: number
}

/**
 * 某个上游最近的若干次拉取。
 *
 * 参数里只有 upstream_id：读哪个日志文件由后端的 configs 决定，界面**不给文件名**
 * ——让调用方指定路径等于把管理接口变成一条任意文件读取的路。条数的上限也在后端，
 * 这里不传 limit，用它的默认值。
 */
export function fetchRecentRequests(key: string, upstreamID: number, signal?: AbortSignal) {
  return adminGet<{ list: RecentRequestItem[] }>(
    `/api/v1/admin/logs/requests?upstream_id=${upstreamID}`,
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

// ── 管理端的写操作 ────────────────────────────────────────────────────
//
// 读和写的失败不是同一组：写还会被后端按业务规则挡下来（不认识的设置项、超出
// 取值范围、主机名重复）。那种失败带一个稳定的错误码，界面按码查自己的文案——
// 后端的 msg 一个字都不往外贴。

/** 一次管理写操作的结果。rejected 带着后端的业务码，由 lib/errors 翻成人话。 */
export type MutationResult<T> =
  | { ok: true; data: T }
  | { ok: false; reason: 'unauthorized' }
  | { ok: false; reason: 'unreachable' }
  | { ok: false; reason: 'rejected'; code: number }

async function adminSend<T>(
  path: string,
  key: string,
  method: 'POST' | 'DELETE',
  payload?: unknown
): Promise<MutationResult<T>> {
  try {
    const resp = await fetch(path, {
      method,
      headers: {
        Accept: 'application/json',
        Authorization: `Bearer ${key}`,
        ...(payload === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: payload === undefined ? undefined : JSON.stringify(payload),
    })
    if (resp.status === 401) {
      return { ok: false, reason: 'unauthorized' }
    }
    const body = (await resp.json()) as Envelope<T>
    if (!resp.ok || body.code !== 0) {
      // 4xx 带着业务码：那是「这次改动本身不成立」，和够不到后端是两件事。
      return typeof body.code === 'number' && body.code > 0
        ? { ok: false, reason: 'rejected', code: body.code }
        : { ok: false, reason: 'unreachable' }
    }
    return { ok: true, data: body.data }
  } catch {
    return { ok: false, reason: 'unreachable' }
  }
}

/** 一条访问规则，取值与后端 api/admin.RuleItem 一致。upstream_id 为 0 即全局规则。 */
export interface AdminRuleItem {
  id: number
  upstream_id: number
  action: RuleAction
  pattern: string
  note: string
  createtime: number
  updatetime: number
}

/** 规则动作，与后端 rule_entity 的取值一致。 */
export type RuleAction = 'allow' | 'deny'

/** 判定发生在哪一层，与后端 admin.Scope 一致。 */
export type RuleScope = 'global' | 'upstream' | 'default'

/** 上游的默认策略，与后端 upstream_entity 的取值一致。 */
export type DefaultPolicy = 'allow_all' | 'deny_unless_matched'

/** 试算过程里的一步。整个过程里至多有一步 decisive 为真。 */
export interface RuleTraceStep {
  scope: RuleScope
  rule_id: number
  pattern: string
  action: RuleAction
  matched: boolean
  decisive: boolean
}

/** 试算结果：判定、是哪条规则决定的、以及完整的求值过程。 */
export interface RuleTestResult {
  host: string
  path: string
  allowed: boolean
  scope: RuleScope
  matched_rule: AdminRuleItem | null
  default_policy: DefaultPolicy
  trace: RuleTraceStep[]
}

/** 一个缓存对象，取值与后端 api/admin.CacheObjectItem 一致。 */
export interface CacheObjectItem {
  id: number
  upstream_id: number
  key: string
  digest: string
  size: number
  immutable: boolean
  pinned: boolean
  expires_at: number
  last_access_at: number
  hit_count: number
  createtime: number
  updatetime: number
}

/** 缓存搜索的一页。page/size 是后端归一化之后的值，分页器按它画。 */
export interface CacheSearchResult {
  list: CacheObjectItem[]
  total: number
  page: number
  size: number
}

/** 清缓存清掉了几条、因为被固定而留下几条。 */
export interface PurgeResult {
  removed: number
  skipped: number
}

/** 一项运行时设置的值类型，界面按它选控件。 */
export type SettingValueType = 'bool' | 'int' | 'string'

/** 一项运行时设置。value 是后端给的 JSON 值本身（字符串、整数或布尔）。 */
export interface SettingItem {
  key: string
  value: unknown
  type: SettingValueType
}

/** 上游的登记信息，写回去时字段要凑齐：保存是整条覆盖，不是打补丁。 */
export interface UpstreamDraft {
  id: number
  host: string
  kind: UpstreamKind
  origin: string
  enabled: boolean
  immutable_patterns: string[]
  mutable_ttl_seconds: number
  default_policy: DefaultPolicy
  library_completion: boolean
  note: string
}

export function fetchRules(key: string, signal?: AbortSignal) {
  return adminGet<{ list: AdminRuleItem[] }>('/api/v1/admin/rules', key, signal)
}

export function saveRule(
  key: string,
  rule: { id: number; upstream_id: number; action: RuleAction; pattern: string; note: string }
) {
  return adminSend<{ id: number }>('/api/v1/admin/rules', key, 'POST', rule)
}

export function deleteRule(key: string, id: number) {
  return adminSend<Record<string, never>>(`/api/v1/admin/rules/${id}`, key, 'DELETE')
}

/**
 * 试算一条资源地址。
 *
 * host 留空时后端按拉取路径的形态解析整条地址，所以上游页上带 host、
 * 全局那一面不带，粘一条完整的 katch 地址两边都认。
 */
export function testRule(key: string, host: string, path: string) {
  return adminSend<RuleTestResult>('/api/v1/admin/rules/test', key, 'POST', { host, path })
}

export function saveUpstream(key: string, upstream: UpstreamDraft) {
  return adminSend<{ id: number }>('/api/v1/admin/upstreams', key, 'POST', upstream)
}

export function searchCacheObjects(
  key: string,
  query: { keyword: string; upstreamID: number; page: number },
  signal?: AbortSignal
) {
  const params = new URLSearchParams({ page: String(query.page) })
  if (query.keyword) {
    params.set('keyword', query.keyword)
  }
  if (query.upstreamID > 0) {
    params.set('upstream_id', String(query.upstreamID))
  }
  return adminGet<CacheSearchResult>(`/api/v1/admin/cache/objects?${params}`, key, signal)
}

/** 清缓存：给 id 清一条，给 upstreamID 清整个上游（固定过的会留下）。 */
export function purgeCache(key: string, target: { id?: number; upstreamID?: number }) {
  return adminSend<PurgeResult>('/api/v1/admin/cache/purge', key, 'POST', {
    id: target.id ?? 0,
    upstream_id: target.upstreamID ?? 0,
  })
}

export function pinCacheObject(key: string, id: number, pinned: boolean) {
  return adminSend<Record<string, never>>(`/api/v1/admin/cache/objects/${id}/pin`, key, 'POST', {
    pinned,
  })
}

export function fetchSettings(key: string, signal?: AbortSignal) {
  return adminGet<{ list: SettingItem[] }>('/api/v1/admin/settings', key, signal)
}

/** 写若干设置，只写给出的那些键。后端整批校验，不会写进去半套。 */
export function saveSettings(key: string, settings: Record<string, unknown>) {
  return adminSend<{ list: SettingItem[] }>('/api/v1/admin/settings', key, 'POST', { settings })
}

export function rotateAdminKey(key: string, newKey: string) {
  return adminSend<Record<string, never>>('/api/v1/admin/settings/admin-key', key, 'POST', {
    new_key: newKey,
  })
}

/**
 * 带密钥取公开上游列表。
 *
 * 缓存页要的是「每个上游占了多少」，而按上游分的缓存量只有这个接口给。它默认
 * 公开，站长关掉公开首页之后匿名调用方拿 404，带密钥的照样能读。
 */
export function fetchUpstreamCacheSizes(key: string, signal?: AbortSignal) {
  return adminGet<UpstreamList>('/api/v1/upstreams', key, signal)
}

/**
 * 库里那条上游整理成写回去的形态。
 *
 * 保存是整条覆盖而不是打补丁，所以写回去的字段必须凑齐；createtime 这类只读字段
 * 不进请求体——它们不是调用方能决定的东西。默认策略可能是空串（从没配过），
 * 按后端 upstream_entity.PolicyAllowAll 的兜底补上。
 */
export function toUpstreamDraft(upstream: AdminUpstreamItem): UpstreamDraft {
  return {
    id: upstream.id,
    host: upstream.host,
    kind: upstream.kind,
    origin: upstream.origin,
    enabled: upstream.enabled,
    immutable_patterns: upstream.immutable_patterns,
    mutable_ttl_seconds: upstream.mutable_ttl_seconds,
    default_policy:
      upstream.default_policy === 'deny_unless_matched' ? 'deny_unless_matched' : 'allow_all',
    library_completion: upstream.library_completion,
    note: upstream.note,
  }
}
