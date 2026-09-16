import { useCallback, useEffect, useRef, useState } from 'react'

import {
  fetchAdminEvents,
  fetchAdminOverview,
  fetchAdminUpstreams,
  fetchCacheImages,
  fetchCacheTree,
  fetchGitMirrors,
  fetchRecentRequests,
  fetchRules,
  fetchSettings,
  fetchUpstreamCacheSizes,
  fetchUpstreamSeries,
  fetchUpstreamStats,
  searchCacheTree,
  type AdminResult,
  type AdminRuleItem,
  type AdminUpstreamItem,
  type CacheImageItem,
  type CacheTreeNode,
  type CacheTreeSearchResult,
  type EventItem,
  type GitMirrorItem,
  type RecentRequestItem,
  type SettingItem,
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
  /** 改完上游（新增、编辑、启停）之后重新取一遍，让侧栏和详情一起跟上。 */
  reload: () => void
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
  const [data, setData] = useState<{
    upstreams: AdminUpstreamItem[]
    stats: UpstreamStatItem[]
  }>({ upstreams: [], stats: [] })
  const [token, reload] = useReloadToken()

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
  }, [key, range, token, reject])

  return { ...data, reload }
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
        // 同 useUpstreamSeries：判的是「这次问到了没有」，空列表也是一个答复。
        const data = unwrap(result, () => reject.current())
        if (data) {
          next[upstreamIDs[index]] = data.list ?? []
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
      const data = unwrap(result, () => reject.current())
      if (data) {
        // 判的是「这次问到了没有」，不是「列表里有没有东西」。拿 list 本身当条件的话，
        // 后端哪天给回一个 null 列表，这里就永远停在「还没问到」——图和回源原因那块
        // 都不画，也不说任何话，而且不会重试。空列表是一个答复，要收下。
        setLoaded({ id: upstreamID, points: data.list ?? [] })
      }
    })
    return () => controller.abort()
  }, [key, upstreamID, range, reject])

  // 上一个上游的序列不许在这一个的图上出现：切换上游时手里那份数据属于别人，
  // 所以这里比对 id 而不是在 effect 里先把状态清空。
  return loaded && loaded.id === upstreamID ? loaded.points : null
}

/**
 * 一个上游最近的几次拉取，还没取到时是 null。
 *
 * 和 useUpstreamSeries 同一套约定：null 是「还没问到」，空数组是「问到了，没有内容」。
 * 「读不到」不再往下细分——库里没有这个上游的行、库读不出来、够不到后端，对调用方是同一
 * 件事；只有 401 单独交回去，那是密钥的事。怎么呈现由调用方决定（最近请求那块面板
 * 的选择是整块不渲染）。
 *
 * 它和 useUpstreamSeries 放在一起，是因为两者要的是同一套机制：id 不合法就不取、
 * 取数可取消、401 单独交回、切换上游时上一个的数据不许留在这一个的界面上。各写一份
 * 的代价不是多几行，而是两份会分家——它们确实分过一次家：一边把 null 列表当成
 * 「没有内容」，另一边当成「还没问到」并从此停在那里。
 */
export function useRecentRequests(
  adminKey: string,
  upstreamID: number,
  onUnauthorized: () => void
): RecentRequestItem[] | null {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [loaded, setLoaded] = useState<{ id: number; list: RecentRequestItem[] } | null>(null)

  useEffect(() => {
    if (!Number.isFinite(upstreamID) || upstreamID <= 0) {
      return
    }
    const controller = new AbortController()
    void fetchRecentRequests(adminKey, upstreamID, controller.signal).then((result) => {
      if (controller.signal.aborted) {
        return
      }
      const data = unwrap(result, () => reject.current())
      if (data) {
        setLoaded({ id: upstreamID, list: data.list ?? [] })
      }
    })
    return () => controller.abort()
  }, [adminKey, upstreamID, reject])

  // 上一个上游的行不许留在这一个的表上：手里那份数据属于别人。
  return loaded && loaded.id === upstreamID ? loaded.list : null
}

/**
 * 一份能被写操作刷新的数据。
 *
 * 管理页改完要看到改完的样子，所以每个取数钩子都给一个 reload：写成功之后调它，
 * 而不是在本地把那条记录改一遍——本地改一遍等于前端第二次实现后端的写入语义，
 * 两边一旦不一致，界面上看到的就是一个后端没有的状态。
 */
export interface Reloadable<T> {
  data: T
  reload: () => void
}

function useReloadToken(): [number, () => void] {
  const [token, setToken] = useState(0)
  const reload = useCallback(() => setToken((value) => value + 1), [])
  return [token, reload]
}

/** 全部访问规则（全局的与各上游的）。规则规模是几十条，后端不分页。 */
export function useAdminRules(
  key: string,
  onUnauthorized: () => void
): Reloadable<AdminRuleItem[]> {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [rules, setRules] = useState<AdminRuleItem[]>([])
  const [token, reload] = useReloadToken()

  useEffect(() => {
    const controller = new AbortController()
    void fetchRules(key, controller.signal).then((result) => {
      if (!controller.signal.aborted) {
        setRules(unwrap(result, () => reject.current())?.list ?? [])
      }
    })
    return () => controller.abort()
  }, [key, token, reject])

  return { data: rules, reload }
}

/** 目录树里已经取到的一层：到目前为止接起来的子项与下一批从哪儿接。 */
export interface CacheTreeLayer {
  totalCount: number
  totalPinned: number
  totalSize: number
  children: CacheTreeNode[]
  hasMore: boolean
  nextOffset: number
}

export interface CacheTreeView {
  /** 当前目录那一层，还没问到时是 null。读不到（目录不合法、够不到后端）按空层给。 */
  current: CacheTreeLayer | null
  /** 就地展开的目录各自的那一层，按目录路径取；还在取的不在表里。 */
  layers: Record<string, CacheTreeLayer>
  /** 就地展开着的目录路径。 */
  expanded: string[]
  /** 展开或收起一个目录。 */
  toggle: (path: string) => void
  /** 把某一层的下一批接在后面。 */
  loadMore: (path: string) => void
  /** 写操作之后重取当前目录与展开着的各层（各自从第一批取起）。 */
  reload: () => void
}

const EMPTY_LAYER: CacheTreeLayer = {
  totalCount: 0,
  totalPinned: 0,
  totalSize: 0,
  children: [],
  hasMore: false,
  nextOffset: 0,
}

interface TreeState {
  /** 这份状态属于哪个当前目录：换了目录，手里展开的那些就属于别人了。 */
  root: string
  layers: Record<string, CacheTreeLayer>
  expanded: string[]
}

/**
 * 缓存对象目录树：当前目录一层，加上就地展开的那些目录各自的一层。
 *
 * 一次只问一层、一层一批，由服务端聚合：APT 的 pool/main/ 这种目录可以有上万个子项，
 * 前端拿平铺结果自己拼树的话，每层的对象数和体积在只拿到一页时都是错的。
 */
export function useCacheTree(key: string, path: string, onUnauthorized: () => void): CacheTreeView {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [state, setState] = useState<TreeState>({ root: path, layers: {}, expanded: [] })
  const [token, reload] = useReloadToken()
  const latest = useRef(state)
  useEffect(() => {
    latest.current = state
  }, [state])

  const load = useCallback(
    (root: string, dir: string, offset: number, signal?: AbortSignal) =>
      fetchCacheTree(key, dir, offset, signal).then((result) => {
        if (signal?.aborted) {
          return
        }
        const data = unwrap(result, () => reject.current())
        setState((current) => {
          const base =
            current.root === root ? current : { root, layers: {}, expanded: [] as string[] }
          const previous = base.layers[dir]
          if (!data) {
            // 翻下一批失败时留着已经接上的那些；第一批就读不到，这一层按空的给。
            return offset > 0 && previous
              ? base
              : { ...base, layers: { ...base.layers, [dir]: EMPTY_LAYER } }
          }
          const children = data.children ?? []
          const layer: CacheTreeLayer = {
            totalCount: data.total_count,
            totalPinned: data.total_pinned,
            totalSize: data.total_size,
            children: offset > 0 && previous ? [...previous.children, ...children] : children,
            hasMore: data.has_more,
            nextOffset: data.next_offset,
          }
          return { ...base, layers: { ...base.layers, [dir]: layer } }
        })
      }),
    [key, reject]
  )

  useEffect(() => {
    const controller = new AbortController()
    const snapshot = latest.current
    const expanded = snapshot.root === path ? snapshot.expanded : []
    for (const dir of [path, ...expanded]) {
      void load(path, dir, 0, controller.signal)
    }
    return () => controller.abort()
  }, [path, token, load])

  const toggle = useCallback(
    (dir: string) => {
      const snapshot = latest.current
      const open = snapshot.root === path && snapshot.expanded.includes(dir)
      setState((current) => {
        const base = current.root === path ? current : { root: path, layers: {}, expanded: [] }
        if (open) {
          // 收起时把这一层扔掉：再展开时重新问一遍，不拿一份可能已经过时的给人看。
          const layers = { ...base.layers }
          delete layers[dir]
          return { ...base, layers, expanded: base.expanded.filter((item) => item !== dir) }
        }
        return { ...base, expanded: [...base.expanded, dir] }
      })
      if (!open) {
        void load(path, dir, 0)
      }
    },
    [path, load]
  )

  const loadMore = useCallback(
    (dir: string) => {
      const layer = latest.current.root === path ? latest.current.layers[dir] : undefined
      if (layer?.hasMore) {
        void load(path, dir, layer.nextOffset)
      }
    },
    [path, load]
  )

  const own = state.root === path
  return {
    current: own ? (state.layers[path] ?? null) : null,
    layers: own ? state.layers : {},
    expanded: own ? state.expanded : [],
    toggle,
    loadMore,
    reload,
  }
}

/**
 * 在一个目录下搜索，没有搜索词时是 null，还没问到时也是 null。
 *
 * 结果带着它是哪个目录、哪个词问出来的：换了词或目录之后，上一次的结果不许挂在
 * 这一次的输入下面。
 */
export function useCacheTreeSearch(
  key: string,
  path: string,
  keyword: string,
  onUnauthorized: () => void
): Reloadable<CacheTreeSearchResult | null> {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [loaded, setLoaded] = useState<{
    path: string
    keyword: string
    result: CacheTreeSearchResult
  } | null>(null)
  const [token, reload] = useReloadToken()

  useEffect(() => {
    if (keyword === '') {
      return
    }
    const controller = new AbortController()
    void searchCacheTree(key, path, keyword, controller.signal).then((response) => {
      if (controller.signal.aborted) {
        return
      }
      const data = unwrap(response, () => reject.current())
      // 读不到（比如词过长被拒）按没有匹配给，而不是停在「还没问到」。
      setLoaded({
        path,
        keyword,
        result: data ?? { path, matched: 0, truncated: false, objects: [], dirs: [] },
      })
    })
    return () => controller.abort()
  }, [key, path, keyword, token, reject])

  const own = keyword !== '' && loaded?.path === path && loaded.keyword === keyword
  return { data: own ? loaded.result : null, reload }
}

/** 按上游分的缓存占用（主机名 → 字节数），容量条按它分段。清缓存之后要跟着刷新。 */
export function useUpstreamCacheSizes(
  key: string,
  onUnauthorized: () => void
): Reloadable<{ host: string; cacheBytes: number }[]> {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [sizes, setSizes] = useState<{ host: string; cacheBytes: number }[]>([])
  const [token, reload] = useReloadToken()

  useEffect(() => {
    const controller = new AbortController()
    void fetchUpstreamCacheSizes(key, controller.signal).then((result) => {
      if (controller.signal.aborted) {
        return
      }
      const list = unwrap(result, () => reject.current())?.list ?? []
      setSizes(list.map((item) => ({ host: item.host, cacheBytes: item.cache_bytes })))
    })
    return () => controller.abort()
  }, [key, token, reject])

  return { data: sizes, reload }
}

/** 全部运行时设置，还没读到时是 null。 */
export function useAdminSettings(
  key: string,
  onUnauthorized: () => void
): Reloadable<SettingItem[] | null> {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [settings, setSettings] = useState<SettingItem[] | null>(null)
  const [token, reload] = useReloadToken()

  useEffect(() => {
    const controller = new AbortController()
    void fetchSettings(key, controller.signal).then((result) => {
      if (!controller.signal.aborted) {
        setSettings(unwrap(result, () => reject.current())?.list ?? null)
      }
    })
    return () => controller.abort()
  }, [key, token, reject])

  return { data: settings, reload }
}

export interface CacheImagesView {
  data: CacheImageItem[]
  total: number
  hasMore: boolean
  /** 把下一批接在后面。 */
  loadMore: () => void
  /** 写操作之后重取（从第一批取起）。 */
  reload: () => void
}

interface ImagesState {
  upstreamID: number
  keyword: string
  list: CacheImageItem[]
  total: number
  hasMore: boolean
  nextOffset: number
}

const EMPTY_IMAGES: ImagesState = {
  upstreamID: 0,
  keyword: '',
  list: [],
  total: 0,
  hasMore: false,
  nextOffset: 0,
}

/**
 * 按仓库列出镜像，一页一批、由服务端聚合（同 useCacheTree 的理由）。
 *
 * 换了上游筛选或搜索词就是另外一次查询：旧一批已经接起来的列表属于上一次的
 * 参数组合，不能借着「加载更多」接在新参数的结果后面。
 */
export function useCacheImages(
  key: string,
  upstreamID: number,
  keyword: string,
  onUnauthorized: () => void
): CacheImagesView {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [state, setState] = useState<ImagesState>({ ...EMPTY_IMAGES, upstreamID, keyword })
  const [token, reload] = useReloadToken()
  const latest = useRef(state)
  useEffect(() => {
    latest.current = state
  }, [state])

  const load = useCallback(
    (targetUpstreamID: number, targetKeyword: string, offset: number, signal?: AbortSignal) =>
      fetchCacheImages(
        key,
        { upstreamID: targetUpstreamID, keyword: targetKeyword, offset },
        signal
      ).then((result) => {
        if (signal?.aborted) {
          return
        }
        const data = unwrap(result, () => reject.current())
        setState((current) => {
          const own = current.upstreamID === targetUpstreamID && current.keyword === targetKeyword
          const previous = own ? current.list : []
          if (!data) {
            return offset > 0 && own
              ? current
              : { ...EMPTY_IMAGES, upstreamID: targetUpstreamID, keyword: targetKeyword }
          }
          const list = data.list ?? []
          return {
            upstreamID: targetUpstreamID,
            keyword: targetKeyword,
            list: offset > 0 && own ? [...previous, ...list] : list,
            total: data.total,
            hasMore: data.has_more,
            nextOffset: data.next_offset,
          }
        })
      }),
    [key, reject]
  )

  useEffect(() => {
    const controller = new AbortController()
    void load(upstreamID, keyword, 0, controller.signal)
    return () => controller.abort()
  }, [upstreamID, keyword, token, load])

  const loadMore = useCallback(() => {
    const snapshot = latest.current
    if (snapshot.upstreamID === upstreamID && snapshot.keyword === keyword && snapshot.hasMore) {
      void load(upstreamID, keyword, snapshot.nextOffset)
    }
  }, [upstreamID, keyword, load])

  const own = state.upstreamID === upstreamID && state.keyword === keyword
  return {
    data: own ? state.list : [],
    total: own ? state.total : 0,
    hasMore: own ? state.hasMore : false,
    loadMore,
    reload,
  }
}

/** 全部 git 本地镜像。规模有配额顶着，后端不分页（同 useAdminRules）。 */
export function useGitMirrors(
  key: string,
  onUnauthorized: () => void
): Reloadable<GitMirrorItem[]> {
  const reject = useRejectOnUnauthorized(onUnauthorized)
  const [mirrors, setMirrors] = useState<GitMirrorItem[]>([])
  const [token, reload] = useReloadToken()

  useEffect(() => {
    const controller = new AbortController()
    void fetchGitMirrors(key, controller.signal).then((result) => {
      if (!controller.signal.aborted) {
        setMirrors(unwrap(result, () => reject.current())?.list ?? [])
      }
    })
    return () => controller.abort()
  }, [key, token, reject])

  return { data: mirrors, reload }
}
