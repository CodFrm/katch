import { HardDrive, Search } from 'lucide-react'
import { Fragment, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { DirRow, LoadMoreRow, ObjectRow } from '@/components/admin/cache-tree'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { useAdminAction } from '@/hooks/use-admin-action'
import {
  useAdminSettings,
  unwrap,
  useCacheTree,
  useCacheTreeSearch,
  useUpstreamCacheSizes,
} from '@/hooks/use-admin-data'
import {
  fetchCacheTree,
  pinCacheObject,
  purgeCache,
  purgeCacheTree,
  type CacheTreeObjectItem,
  type CacheTreeSearchDir,
  type CacheTreeSearchObject,
  type PurgeResult,
} from '@/lib/api'
import { formatBytes, formatCount } from '@/lib/format'
import { readIntSetting } from '@/lib/settings'
import { cn } from '@/lib/utils'

/** 容量条上每一段的颜色，按上游在列表里的次序取。分不清谁是谁的色条没有意义。 */
const SEGMENT_TONES = ['bg-primary', 'bg-ok', 'bg-warn', 'bg-line-strong', 'bg-ink-3']

/** 搜索的节流：每敲一个字就打一次库，对一张几十万行的表是自找的压力。 */
const SEARCH_DEBOUNCE_MS = 250

/** 搜索词的上限，同后端 CacheTreeSearchRequest.Keyword。 */
const KEYWORD_MAX = 256

/** 路径导航里段与段之间的分隔。 */
const CRUMB_SEPARATOR = '/'

/** 一个目录路径的上一级；主机那一层的上一级是根（空串）。目录路径里没有查询串。 */
function parentOf(path: string): string {
  const at = path.lastIndexOf('/')
  return at < 0 ? '' : path.slice(0, at)
}

/** 搜索结果按上级目录归组，好从当前目录起一层层画成树。 */
interface SearchLevel {
  dirs: CacheTreeSearchDir[]
  objects: CacheTreeSearchObject[]
}

function groupSearch(dirs: CacheTreeSearchDir[], objects: CacheTreeSearchObject[]) {
  const levels = new Map<string, SearchLevel>()
  const levelOf = (parent: string) => {
    let level = levels.get(parent)
    if (!level) {
      level = { dirs: [], objects: [] }
      levels.set(parent, level)
    }
    return level
  }
  for (const dir of dirs) {
    levelOf(parentOf(dir.path)).dirs.push(dir)
  }
  for (const object of objects) {
    // 对象名里可能带着含 / 的查询串，所以上级按名字的长度切，不按最后一个 / 切。
    levelOf(object.path.slice(0, object.path.length - object.name.length - 1)).objects.push(object)
  }
  for (const level of levels.values()) {
    level.dirs.sort((a, b) => a.path.localeCompare(b.path))
    level.objects.sort((a, b) => a.name.localeCompare(b.name))
  }
  return levels
}

/** 等着确认的按目录清除。count 是将清除的未固定对象数，还没问到时是 null；failed 是没问到。 */
interface PurgeTarget {
  path: string
  count: number | null
  failed?: boolean
}

/**
 * 缓存对象：顶上是按上游分段的容量条，底下是可展开、可进入的目录树。
 *
 * 当前目录挂在地址的 ?path= 上，刷新和分享链接能回到同一层，浏览器后退回到上一层。
 * 树一次只问一层、搜索只在当前目录下进行，都交给后端：对象是拉取路径自己长出来的，
 * 一个跑了几天的镜像站就有几十万条。
 */
export function CacheScreen({
  adminKey,
  onUnauthorized,
}: {
  adminKey: string
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const [params, setParams] = useSearchParams()
  const path = params.get('path') ?? ''
  const [keyword, setKeyword] = useState('')
  const [committed, setCommitted] = useState('')
  const [selected, setSelected] = useState<number[]>([])
  const [collapsed, setCollapsed] = useState<{ keyword: string; paths: string[] }>({
    keyword: '',
    paths: [],
  })
  const [target, setTarget] = useState<PurgeTarget | null>(null)
  const [purged, setPurged] = useState<PurgeResult | null>(null)
  const action = useAdminAction(onUnauthorized)
  const sizes = useUpstreamCacheSizes(adminKey, onUnauthorized)
  const settings = useAdminSettings(adminKey, onUnauthorized)
  const tree = useCacheTree(adminKey, path, onUnauthorized)
  const search = useCacheTreeSearch(adminKey, path, committed, onUnauthorized)
  const searching = committed !== ''

  useEffect(() => {
    const timer = setTimeout(() => setCommitted(keyword.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [keyword])

  // 搜索结果里的目录行没有「其中几个已固定」，确认前要单独问一次那一层的合计。
  const pendingCount =
    target !== null && target.count === null && !target.failed ? target.path : null
  useEffect(() => {
    if (pendingCount === null) {
      return
    }
    const controller = new AbortController()
    void fetchCacheTree(adminKey, pendingCount, 0, controller.signal).then((result) => {
      if (controller.signal.aborted) {
        return
      }
      const data = unwrap(result, onUnauthorized)
      if (data) {
        const count = data.total_count - data.total_pinned
        setTarget((current) => (current?.path === pendingCount ? { ...current, count } : current))
      } else {
        // 问不到合计就说不出将清除几个：确认按钮点不了，得把原因说出来。
        setTarget((current) =>
          current?.path === pendingCount ? { ...current, failed: true } : current
        )
      }
    })
    return () => controller.abort()
  }, [adminKey, pendingCount, onUnauthorized])

  const levels = useMemo(
    () => (search.data ? groupSearch(search.data.dirs ?? [], search.data.objects ?? []) : null),
    [search.data]
  )

  const current = tree.current
  const expandedLayers = tree.layers
  // 看得见的对象按 id 收一份：批量按钮要知道选中的是不是全都固定着。
  const visible = useMemo(() => {
    const table = new Map<number, CacheTreeObjectItem>()
    const layers = [current, ...Object.values(expandedLayers)]
    for (const layer of layers) {
      for (const child of layer?.children ?? []) {
        if (child.kind === 'object') {
          table.set(child.object.id, child.object)
        }
      }
    }
    for (const item of search.data?.objects ?? []) {
      table.set(item.object.id, item.object)
    }
    return table
  }, [current, expandedLayers, search.data])

  const quota = readIntSetting(settings.data, 'cache_quota_bytes')
  const watermark = readIntSetting(settings.data, 'cache_reclaim_percent')
  const used = sizes.data.reduce((sum, item) => sum + item.cacheBytes, 0)
  const segments = path === '' ? [] : path.split('/')
  // 读不出来的那一层或那次搜索用管理操作的错误提示说出来；写操作的错误更具体，先说它。
  const loadErrorKey = searching
    ? search.failed
      ? 'admin.cache.tree.searchFailed'
      : null
    : tree.failed
      ? 'admin.cache.tree.loadFailed'
      : null
  const errorKey =
    action.errorKey ?? (target?.failed ? 'admin.cache.tree.loadFailed' : loadErrorKey)

  // 选中的全是已固定的对象时，这枚按钮换成放开——只给 pin 不给 unpin 的界面
  // 会让一个钉住的对象再也清不掉。
  const unpinning = selected.length > 0 && selected.every((id) => visible.get(id)?.pinned === true)

  function navigate(next: string) {
    const query = new URLSearchParams(params)
    if (next === '') {
      query.delete('path')
    } else {
      query.set('path', next)
    }
    setParams(query)
    setSelected([])
    setTarget(null)
    setPurged(null)
  }

  function toggle(id: number) {
    setSelected((items) =>
      items.includes(id) ? items.filter((item) => item !== id) : [...items, id]
    )
  }

  /** 清完、固定完之后，树、搜索结果和容量条都要跟上。 */
  function refresh() {
    tree.reload()
    search.reload()
    sizes.reload()
  }

  /** 批量操作是一条条发的：后端按 id 只收一条，没有「这一批」的端点。 */
  async function purgeMany(ids: number[]) {
    for (const id of ids) {
      await action.run(() => purgeCache(adminKey, { id }))
    }
    setSelected([])
    refresh()
  }

  async function pinMany(ids: number[], pinned: boolean) {
    for (const id of ids) {
      await action.run(() => pinCacheObject(adminKey, id, pinned))
    }
    setSelected([])
    refresh()
  }

  function askPurge(dir: string, count: number | null) {
    action.clearError()
    setPurged(null)
    setTarget({ path: dir, count })
  }

  function purgeDir(dir: string) {
    void action.run(
      () => purgeCacheTree(adminKey, dir),
      (result) => {
        setTarget(null)
        setPurged(result)
        setSelected([])
        refresh()
      }
    )
  }

  function toggleSearchDir(dir: string) {
    setCollapsed((state) => {
      const paths = state.keyword === committed ? state.paths : []
      return {
        keyword: committed,
        paths: paths.includes(dir) ? paths.filter((item) => item !== dir) : [...paths, dir],
      }
    })
  }

  function nameOf(dir: string) {
    return dir.slice(dir.lastIndexOf('/') + 1)
  }

  function renderLayer(dir: string, depth: number): ReactNode {
    const layer = dir === path ? current : tree.layers[dir]
    if (!layer) {
      return null
    }
    return (
      <>
        {layer.children.map((child) =>
          child.kind === 'dir' ? (
            <Fragment key={`d:${child.path}`}>
              <DirRow
                name={child.name}
                path={child.path}
                depth={depth}
                expanded={tree.expanded.includes(child.path)}
                count={child.count}
                size={child.size}
                lastAccessAt={child.last_access_at}
                keyword=""
                onToggle={() => tree.toggle(child.path)}
                onEnter={() => navigate(child.path)}
                onPurge={() => askPurge(child.path, child.count - child.pinned_count)}
              />
              {tree.expanded.includes(child.path) && renderLayer(child.path, depth + 1)}
            </Fragment>
          ) : (
            <ObjectRow
              key={`o:${child.object.id}`}
              name={child.name}
              path={child.path}
              variant={child.variant}
              object={child.object}
              depth={depth}
              keyword=""
              checked={selected.includes(child.object.id)}
              onToggle={() => toggle(child.object.id)}
              onPurge={() => void purgeMany([child.object.id])}
            />
          )
        )}
        {layer.hasMore && <LoadMoreRow depth={depth} onClick={() => tree.loadMore(dir)} />}
      </>
    )
  }

  function renderMatches(dir: string, depth: number): ReactNode {
    const level = levels?.get(dir)
    if (!level) {
      return null
    }
    const hidden = collapsed.keyword === committed ? collapsed.paths : []
    return (
      <>
        {level.dirs.map((item) => {
          const open = !hidden.includes(item.path)
          return (
            <Fragment key={`d:${item.path}`}>
              <DirRow
                name={nameOf(item.path)}
                path={item.path}
                depth={depth}
                expanded={open}
                count={item.matched_count}
                size={item.matched_size}
                lastAccessAt={item.last_access_at}
                matched={item.matched_count}
                keyword={committed}
                onToggle={() => toggleSearchDir(item.path)}
                onEnter={() => navigate(item.path)}
                onPurge={() => askPurge(item.path, null)}
              />
              {open && renderMatches(item.path, depth + 1)}
            </Fragment>
          )
        })}
        {level.objects.map((item) => (
          <ObjectRow
            key={`o:${item.object.id}`}
            name={item.name}
            path={item.path}
            variant={item.variant}
            object={item.object}
            depth={depth}
            keyword={committed}
            checked={selected.includes(item.object.id)}
            onToggle={() => toggle(item.object.id)}
            onPurge={() => void purgeMany([item.object.id])}
          />
        ))}
      </>
    )
  }

  const total = formatBytes(current?.totalSize ?? 0)

  return (
    <>
      <ScreenHeader
        title={t('admin.cache.title')}
        subtitle={
          <>
            <span className="text-muted-foreground font-mono">
              {formatCount(current?.totalCount ?? 0)}
            </span>
            <span className="text-muted-foreground">{t('admin.cache.objects')}</span>
            <span className="text-ink-3">·</span>
            <span className="text-muted-foreground">{t('admin.cache.addressing')}</span>
          </>
        }
      />

      <section className="border-border flex flex-col gap-5 border-b px-8 py-5">
        <div className="flex flex-wrap items-baseline gap-2.5">
          <span className="text-foreground font-mono text-[26px] leading-none tracking-[-0.035em]">
            {formatBytes(used).value}
          </span>
          <span className="text-ink-3 font-mono text-xs">{formatBytes(used).unit}</span>
          <span className="text-muted-foreground font-mono text-[13px]">
            {t('admin.cache.quota', {
              size: `${formatBytes(quota).value} ${formatBytes(quota).unit}`,
            })}
          </span>
          <span className="flex-1" />
          {watermark > 0 && (
            <span className="text-ink-3 text-[11.5px]">
              {t('admin.cache.watermark', { percent: watermark })}
            </span>
          )}
        </div>
        <div
          role="img"
          aria-label={t('admin.cache.capacity')}
          className="border-border bg-background flex h-2.5 w-full border"
        >
          {sizes.data.map((item, index) => (
            <span
              key={item.host}
              data-slot="capacity-segment"
              title={item.host}
              className={cn('h-full', SEGMENT_TONES[index % SEGMENT_TONES.length])}
              style={{
                width: `${quota > 0 ? Math.min(100, (item.cacheBytes / quota) * 100) : 0}%`,
              }}
            />
          ))}
        </div>
        <ul className="flex flex-wrap gap-x-8 gap-y-2">
          {sizes.data.map((item, index) => (
            <li key={item.host} className="flex items-center gap-2">
              <span
                aria-hidden
                className={cn('size-2', SEGMENT_TONES[index % SEGMENT_TONES.length])}
              />
              <span className="text-muted-foreground font-mono text-[11.5px]">{item.host}</span>
              <span className="text-ink-3 font-mono text-[11.5px]">
                {formatBytes(item.cacheBytes).value} {formatBytes(item.cacheBytes).unit}
              </span>
            </li>
          ))}
        </ul>
      </section>

      <div className="flex flex-col gap-4 px-8 py-5">
        <div className="flex flex-wrap items-center gap-2.5">
          <nav
            aria-label={t('admin.cache.tree.crumbs')}
            className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5 font-mono text-[13px]"
          >
            <HardDrive className="text-ink-3 size-3.5 shrink-0" aria-hidden />
            {segments.length === 0 ? (
              <span className="text-foreground">{t('admin.cache.allUpstreams')}</span>
            ) : (
              <button
                type="button"
                onClick={() => navigate('')}
                className="text-muted-foreground hover:text-primary"
              >
                {t('admin.cache.allUpstreams')}
              </button>
            )}
            {segments.map((segment, index) => {
              const last = index === segments.length - 1
              return (
                <Fragment key={index}>
                  <span className="text-ink-3" aria-hidden>
                    {CRUMB_SEPARATOR}
                  </span>
                  {last ? (
                    <span className="text-foreground min-w-0 truncate" title={path}>
                      {segment}
                    </span>
                  ) : (
                    <button
                      type="button"
                      onClick={() => navigate(segments.slice(0, index + 1).join('/'))}
                      className="text-muted-foreground hover:text-primary"
                    >
                      {segment}
                    </button>
                  )}
                </Fragment>
              )
            })}
          </nav>
          {current && (
            <span className="text-muted-foreground font-mono text-[12.5px]">
              {t('admin.cache.tree.summary', {
                count: formatCount(current.totalCount),
                size: `${total.value} ${total.unit}`,
              })}
            </span>
          )}
          {path !== '' && (
            <button
              type="button"
              onClick={() =>
                askPurge(path, current ? current.totalCount - current.totalPinned : null)
              }
              className="border-line-strong text-foreground hover:text-destructive border px-3 py-1.5 text-xs"
            >
              {t('admin.cache.tree.purgeDir')}
            </button>
          )}
        </div>

        <div className="flex flex-wrap items-center gap-2.5">
          <div className="border-line-strong bg-background flex min-w-[280px] flex-1 items-center gap-2 border px-3 py-2">
            <Search className="text-ink-3 size-3.5" aria-hidden />
            <Input
              aria-label={t('admin.cache.search')}
              value={keyword}
              spellCheck={false}
              autoComplete="off"
              maxLength={KEYWORD_MAX}
              placeholder={t('admin.cache.searchPlaceholder')}
              onChange={(event) => setKeyword(event.target.value)}
              className="h-auto rounded-none border-0 bg-transparent p-0 font-mono text-[13px] shadow-none focus-visible:ring-0 md:text-[13px] dark:bg-transparent"
            />
            {searching && search.data && (
              <span className="text-muted-foreground shrink-0 text-[12.5px]">
                {t('admin.cache.found', { total: formatCount(search.data.matched) })}
              </span>
            )}
          </div>
          {selected.length > 0 && (
            <div className="border-primary bg-signal-soft flex items-center gap-3 border px-3 py-2">
              <span className="text-foreground text-[12.5px]">
                {t('admin.cache.selected', { total: selected.length })}
              </span>
              <button
                type="button"
                onClick={() => void purgeMany(selected)}
                className="text-primary text-[12.5px]"
              >
                {t('admin.cache.purge')}
              </button>
              <button
                type="button"
                onClick={() => void pinMany(selected, !unpinning)}
                className="text-primary text-[12.5px]"
              >
                {t(unpinning ? 'admin.cache.unpin' : 'admin.cache.pin')}
              </button>
            </div>
          )}
        </div>

        {target && (
          <div
            role="alertdialog"
            aria-label={t('admin.cache.tree.purgeDir')}
            className="border-destructive flex flex-wrap items-center gap-3 border px-3 py-2"
          >
            <span className="text-foreground min-w-0 flex-1 text-[12.5px] break-all">
              {target.count === null
                ? target.path
                : t('admin.cache.tree.purgeQuestion', {
                    path: target.path,
                    count: formatCount(target.count),
                  })}
            </span>
            <button
              type="button"
              onClick={() => purgeDir(target.path)}
              disabled={action.pending || target.count === null}
              className="bg-destructive text-background px-3.5 py-1.5 text-xs disabled:opacity-45"
            >
              {t('admin.cache.tree.purgeConfirm')}
            </button>
            <button
              type="button"
              onClick={() => setTarget(null)}
              className="text-muted-foreground px-2 py-1.5 text-xs"
            >
              {t('admin.cache.tree.purgeCancel')}
            </button>
          </div>
        )}

        {purged && (
          <p role="status" className="text-muted-foreground text-[12.5px]">
            {t('admin.cache.tree.purged', { removed: purged.removed, skipped: purged.skipped })}
          </p>
        )}

        {errorKey && (
          <p role="alert" className="text-destructive text-[12.5px]">
            {t(errorKey)}
          </p>
        )}

        <Table aria-label={t('admin.cache.title')} className="min-w-[860px] table-fixed text-left">
          <thead>
            <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
              <th scope="col" className="w-7 py-2 font-normal">
                <span className="sr-only">{t('admin.cache.selectColumn')}</span>
              </th>
              <th scope="col" className="py-2 font-normal">
                {t('admin.cache.columns.name')}
              </th>
              <th scope="col" className="w-[90px] py-2 font-normal">
                {t('admin.cache.columns.count')}
              </th>
              <th scope="col" className="w-[100px] py-2 font-normal">
                {t('admin.cache.columns.size')}
              </th>
              <th scope="col" className="w-[90px] py-2 font-normal">
                {t('admin.cache.columns.hits')}
              </th>
              <th scope="col" className="w-[120px] py-2 font-normal">
                {t('admin.cache.columns.lastAccess')}
              </th>
              <th scope="col" className="w-[70px] py-2 font-normal">
                {t('admin.cache.columns.operations')}
              </th>
            </tr>
          </thead>
          <tbody>{searching ? renderMatches(path, 0) : renderLayer(path, 0)}</tbody>
        </Table>
        {searching
          ? search.data &&
            (search.data.objects ?? []).length === 0 && (
              <p className="text-muted-foreground text-[13px]">{t('admin.cache.empty')}</p>
            )
          : current &&
            !tree.failed &&
            current.children.length === 0 && (
              <p className="text-muted-foreground text-[13px]">{t('admin.cache.tree.emptyDir')}</p>
            )}
        {searching && search.data?.truncated && (
          <p className="text-muted-foreground text-[12.5px]">
            {t('admin.cache.tree.truncated', { shown: (search.data.objects ?? []).length })}
          </p>
        )}
      </div>
    </>
  )
}
