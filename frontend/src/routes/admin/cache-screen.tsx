import { Search } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { useAdminAction } from '@/hooks/use-admin-action'
import { useAdminSettings, useCacheObjects, useUpstreamCacheSizes } from '@/hooks/use-admin-data'
import { pinCacheObject, purgeCache, type AdminUpstreamItem, type CacheObjectItem } from '@/lib/api'
import { formatBytes, formatCount, formatStamp } from '@/lib/format'
import { readIntSetting } from '@/lib/settings'
import { cn } from '@/lib/utils'

/** 容量条上每一段的颜色，按上游在列表里的次序取。分不清谁是谁的色条没有意义。 */
const SEGMENT_TONES = ['bg-primary', 'bg-ok', 'bg-warn', 'bg-line-strong', 'bg-ink-3']

/**
 * 摘要在表里只留一小截：整串 sha256 占满一行，而运维要的只是「这两条是不是同一份」。
 * 算法前缀换成 @，那是排版，不是文案。
 */
function shortDigest(digest: string): string {
  return digest.replace('sha256:', '@').slice(0, 10)
}

/** 搜索的节流：每敲一个字就打一次库，对一张几十万行的表是自找的压力。 */
const SEARCH_DEBOUNCE_MS = 250

/**
 * 缓存对象：顶上是按上游分段的容量条，底下是搜索 + 筛选 + 批量操作的对象表。
 *
 * 搜索和筛选都交给后端：对象是拉取路径自己长出来的，一个跑了几天的镜像站就有
 * 几十万条，捞回来在前端过滤既打爆界面也答不出「一共有多少条」。
 */
export function CacheScreen({
  adminKey,
  upstreams,
  onUnauthorized,
}: {
  adminKey: string
  upstreams: AdminUpstreamItem[]
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const [params, setParams] = useSearchParams()
  const upstreamID = Number(params.get('upstream') ?? '0') || 0
  const [keyword, setKeyword] = useState('')
  const [committed, setCommitted] = useState('')
  const [selected, setSelected] = useState<number[]>([])
  const action = useAdminAction(onUnauthorized)
  const sizes = useUpstreamCacheSizes(adminKey, onUnauthorized)
  const settings = useAdminSettings(adminKey, onUnauthorized)
  const page = useCacheObjects(
    adminKey,
    { keyword: committed, upstreamID, page: 1 },
    onUnauthorized
  )

  useEffect(() => {
    const timer = setTimeout(() => setCommitted(keyword.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [keyword])

  const hostOf = useMemo(() => {
    const table = new Map(upstreams.map((item) => [item.id, item.host]))
    return (id: number) => table.get(id) ?? ''
  }, [upstreams])

  const objects = page.data?.list ?? []
  const quota = readIntSetting(settings.data, 'cache_quota_bytes')
  const watermark = readIntSetting(settings.data, 'cache_reclaim_percent')
  const used = sizes.reduce((sum, item) => sum + item.cacheBytes, 0)

  // 选中的全是已固定的对象时，这枚按钮换成放开——只给 pin 不给 unpin 的界面
  // 会让一个钉住的对象再也清不掉。
  const unpinning =
    selected.length > 0 &&
    selected.every((id) => objects.find((object) => object.id === id)?.pinned === true)

  function toggle(id: number) {
    setSelected((current) =>
      current.includes(id) ? current.filter((item) => item !== id) : [...current, id]
    )
  }

  /** 批量操作是一条条发的：后端按 id 只收一条，没有「这一批」的端点。 */
  async function purgeMany(ids: number[]) {
    for (const id of ids) {
      await action.run(() => purgeCache(adminKey, { id }))
    }
    setSelected([])
    page.reload()
  }

  async function pinMany(ids: number[], pinned: boolean) {
    for (const id of ids) {
      await action.run(() => pinCacheObject(adminKey, id, pinned))
    }
    setSelected([])
    page.reload()
  }

  return (
    <>
      <ScreenHeader
        title={t('admin.cache.title')}
        subtitle={
          <>
            <span className="text-muted-foreground font-mono">
              {formatCount(page.data?.total ?? 0)}
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
          {sizes.map((item, index) => (
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
          {sizes.map((item, index) => (
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
          <div className="border-line-strong bg-background flex min-w-[280px] flex-1 items-center gap-2 border px-3 py-2">
            <Search className="text-ink-3 size-3.5" aria-hidden />
            <Input
              aria-label={t('admin.cache.search')}
              value={keyword}
              spellCheck={false}
              autoComplete="off"
              placeholder={t('admin.cache.searchPlaceholder')}
              onChange={(event) => setKeyword(event.target.value)}
              className="h-auto rounded-none border-0 bg-transparent p-0 font-mono text-[13px] shadow-none focus-visible:ring-0 md:text-[13px] dark:bg-transparent"
            />
            <span className="text-muted-foreground shrink-0 text-[12.5px]">
              {t('admin.cache.found', { total: formatCount(page.data?.total ?? 0) })}
            </span>
          </div>
          <select
            aria-label={t('admin.cache.filterUpstream')}
            value={String(upstreamID)}
            onChange={(event) => {
              const next = new URLSearchParams(params)
              if (event.target.value === '0') {
                next.delete('upstream')
              } else {
                next.set('upstream', event.target.value)
              }
              setParams(next)
            }}
            className="border-line-strong bg-background text-foreground border px-3 py-2 text-[12.5px]"
          >
            <option value="0">{t('admin.cache.allUpstreams')}</option>
            {upstreams.map((item) => (
              <option key={item.id} value={String(item.id)}>
                {item.host}
              </option>
            ))}
          </select>
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

        {action.errorKey && (
          <p role="alert" className="text-destructive text-[12.5px]">
            {t(action.errorKey)}
          </p>
        )}

        <Table aria-label={t('admin.cache.title')} className="min-w-[860px] table-fixed text-left">
          <thead>
            <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
              <th scope="col" className="w-7 py-2 font-normal">
                <span className="sr-only">{t('admin.cache.selectColumn')}</span>
              </th>
              <th scope="col" className="py-2 font-normal">
                {t('admin.cache.columns.object')}
              </th>
              <th scope="col" className="w-[170px] py-2 font-normal">
                {t('admin.cache.columns.upstream')}
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
          <tbody>
            {objects.map((object) => (
              <ObjectRow
                key={object.id}
                object={object}
                host={hostOf(object.upstream_id)}
                checked={selected.includes(object.id)}
                onToggle={() => toggle(object.id)}
                onPurge={() => void purgeMany([object.id])}
              />
            ))}
          </tbody>
        </Table>
        {page.data && objects.length === 0 && (
          <p className="text-muted-foreground text-[13px]">{t('admin.cache.empty')}</p>
        )}
      </div>
    </>
  )
}

function ObjectRow({
  object,
  host,
  checked,
  onToggle,
  onPurge,
}: {
  object: CacheObjectItem
  host: string
  checked: boolean
  onToggle: () => void
  onPurge: () => void
}) {
  const { t } = useTranslation()
  const size = formatBytes(object.size)
  const stamp = formatStamp(object.last_access_at)
  return (
    <tr className="border-border border-b">
      <td className="py-2.5">
        <input
          type="checkbox"
          checked={checked}
          onChange={onToggle}
          aria-label={t('admin.cache.selectObject', { key: object.key })}
          className="accent-primary size-3"
        />
      </td>
      <td className="min-w-0 py-2.5">
        <div className="flex min-w-0 items-baseline gap-2">
          <span
            className="text-foreground block min-w-0 flex-1 truncate font-mono text-[13px]"
            title={object.key}
          >
            {object.key}
          </span>
          {object.digest && (
            <span className="text-ink-3 shrink-0 font-mono text-[11px]">
              {shortDigest(object.digest)}
            </span>
          )}
          {object.pinned && (
            <span className="text-primary border-primary shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.cache.pinned')}
            </span>
          )}
        </div>
      </td>
      <td className="text-muted-foreground min-w-0 py-2.5 font-mono text-[13px]">
        <span className="block truncate pr-4" title={host}>
          {host}
        </span>
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {size.value} {size.unit}
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {formatCount(object.hit_count)}
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">
        {stamp.day === 'today'
          ? stamp.time
          : stamp.day === 'yesterday'
            ? t('admin.events.yesterday', { time: stamp.time })
            : `${stamp.date} ${stamp.time}`}
      </td>
      <td className="py-2.5">
        <button
          type="button"
          onClick={onPurge}
          className="text-muted-foreground hover:text-destructive text-[12.5px]"
        >
          {t('admin.cache.purge')}
        </button>
      </td>
    </tr>
  )
}
