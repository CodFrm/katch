import { ChevronDown, ChevronRight, Container, Hash, Tag as TagIcon } from 'lucide-react'
import { Fragment, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { ABSENT, INDENT_PX, shortDigest, Stamp } from '@/components/admin/cache-tree'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { useAdminAction } from '@/hooks/use-admin-action'
import { useCacheImages } from '@/hooks/use-admin-data'
import {
  fetchCacheImageTags,
  purgeCacheImage,
  type AdminUpstreamItem,
  type CacheImageItem,
  type CacheImageTag,
  type PurgeResult,
} from '@/lib/api'
import { formatBytes, formatCount } from '@/lib/format'

/** 搜索的节流：同缓存对象页，一敲一个字就打一次库是自找的压力。 */
const SEARCH_DEBOUNCE_MS = 250

/** 搜索词的上限，同后端 ListCacheImagesRequest.Keyword。 */
const KEYWORD_MAX = 256

/** 一个镜像行在这个页面里的身份：上游加仓库名，两者合在一起才唯一。 */
function rowKey(upstreamID: number, repository: string): string {
  return `${upstreamID}:${repository}`
}

/** 等着确认的删除：镜像或者一个 tag。count 是确认处写明的对象数。 */
type PurgeTarget =
  | { kind: 'image'; upstreamID: number; repository: string; name: string; count: number }
  | {
      kind: 'tag'
      upstreamID: number
      repository: string
      reference: string
      name: string
      count: number
    }

/**
 * 容器镜像：按仓库列出协议含 registry 的上游下缓存过的镜像，展开看 tag。
 *
 * 归并、变体合并、体积口径都在服务端算好（决策 7–10），这里只管把一页镜像画
 * 出来、按需把某一行的全部 tag 问回来。搜索命中 tag 时列表接口已经把命中的
 * 那些 tag 带在行上，不用再问一次；命中仓库名或者用户手动展开时，才去问
 * /images/tags 要全部 tag（同缓存对象页「一次只问一层」的理由：镜像可能有几百
 * 个 tag，没人会一次全看）。
 */
export function ImagesScreen({
  adminKey,
  upstreams,
  onUnauthorized,
}: {
  adminKey: string
  upstreams: AdminUpstreamItem[]
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const [upstreamID, setUpstreamID] = useState(0)
  const [keyword, setKeyword] = useState('')
  const [committed, setCommitted] = useState('')
  const [manualToggle, setManualToggle] = useState<{ keyword: string; keys: string[] }>({
    keyword: '',
    keys: [],
  })
  const [tags, setTags] = useState<Record<string, CacheImageTag[]>>({})
  /** tag 没读出来的那些行：读不出来不等于这个镜像没有 tag，要说出来。 */
  const [tagsFailed, setTagsFailed] = useState<string[]>([])
  const [target, setTarget] = useState<PurgeTarget | null>(null)
  const [purged, setPurged] = useState<PurgeResult | null>(null)
  const action = useAdminAction(onUnauthorized)
  const images = useCacheImages(adminKey, upstreamID, committed, onUnauthorized)
  const registryUpstreams = upstreams.filter((item) => item.protocols.includes('registry'))

  useEffect(() => {
    const timer = setTimeout(() => setCommitted(keyword.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [keyword])

  // 读不出来的列表或 tag 用管理操作的错误提示说出来；写操作的错误更具体，先说它。
  const errorKey =
    action.errorKey ??
    (images.failed
      ? 'admin.images.loadFailed'
      : tagsFailed.length > 0
        ? 'admin.images.tagsFailed'
        : null)

  function isExpanded(item: CacheImageItem): boolean {
    const key = rowKey(item.upstream_id, item.repository)
    const toggled = manualToggle.keyword === committed && manualToggle.keys.includes(key)
    // 搜索命中时默认展开（要么只看命中的 tag，要么看全部）；手动点击的三角是对
    // 这份默认状态的一次翻转，换了搜索词就不再算数。
    return committed !== '' ? !toggled : toggled
  }

  // 展开着、但列表行没带全部 tag（没搜索，或者搜索命中的是仓库名）的那些，要
  // 单独问一次 /images/tags。拼成一串稳定的字符串做依赖：这份列表本身每次渲染
  // 都是新数组，直接放进依赖数组会在每次敲键盘时都重新问一遍。
  const pendingKeys = images.data
    .filter(
      (item) =>
        isExpanded(item) &&
        item.tags.length === 0 &&
        !(rowKey(item.upstream_id, item.repository) in tags)
    )
    .map((item) => rowKey(item.upstream_id, item.repository))
    .join(',')

  useEffect(() => {
    if (pendingKeys === '') {
      return
    }
    const controller = new AbortController()
    for (const key of pendingKeys.split(',')) {
      const item = images.data.find((entry) => rowKey(entry.upstream_id, entry.repository) === key)
      if (!item) {
        continue
      }
      void fetchCacheImageTags(
        adminKey,
        item.upstream_id,
        item.repository,
        '',
        controller.signal
      ).then((result) => {
        if (controller.signal.aborted) {
          return
        }
        if (result.ok) {
          setTagsFailed((current) => current.filter((entry) => entry !== key))
          setTags((current) => ({ ...current, [key]: result.data.list ?? [] }))
        } else if (result.reason === 'unauthorized') {
          onUnauthorized()
        } else {
          setTagsFailed((current) => (current.includes(key) ? current : [...current, key]))
        }
      })
    }
    return () => controller.abort()
  }, [adminKey, onUnauthorized, images.data, pendingKeys])

  function toggleRow(item: CacheImageItem) {
    const key = rowKey(item.upstream_id, item.repository)
    setTagsFailed((current) => current.filter((entry) => entry !== key))
    setManualToggle((state) => {
      const keys = state.keyword === committed ? state.keys : []
      return {
        keyword: committed,
        keys: keys.includes(key) ? keys.filter((entry) => entry !== key) : [...keys, key],
      }
    })
  }

  function refresh() {
    setTagsFailed([])
    images.reload()
  }

  function forgetTags(upstreamID: number, repository: string) {
    const key = rowKey(upstreamID, repository)
    setTags((current) => {
      const next = { ...current }
      delete next[key]
      return next
    })
  }

  function askPurgeImage(item: CacheImageItem) {
    action.clearError()
    setPurged(null)
    setTarget({
      kind: 'image',
      upstreamID: item.upstream_id,
      repository: item.repository,
      name: `${item.host}/${item.repository}`,
      count: item.object_count - item.pinned_count,
    })
  }

  function askPurgeTag(item: CacheImageItem, tag: CacheImageTag) {
    action.clearError()
    setPurged(null)
    setTarget({
      kind: 'tag',
      upstreamID: item.upstream_id,
      repository: item.repository,
      reference: tag.reference,
      name: tag.reference,
      count: tag.object_count - tag.pinned_count,
    })
  }

  function confirmPurge() {
    if (!target) {
      return
    }
    void action.run(
      () =>
        purgeCacheImage(adminKey, {
          upstreamID: target.upstreamID,
          repository: target.repository,
          reference: target.kind === 'tag' ? target.reference : undefined,
        }),
      (result) => {
        setTarget(null)
        setPurged(result)
        forgetTags(target.upstreamID, target.repository)
        refresh()
      }
    )
  }

  return (
    <>
      <ScreenHeader
        title={t('admin.images.title')}
        subtitle={
          <>
            <span className="text-muted-foreground font-mono">{formatCount(images.total)}</span>
            <span className="text-muted-foreground">{t('admin.images.countUnit')}</span>
            <span className="text-ink-3">·</span>
            <span className="text-muted-foreground">{t('admin.images.sizeNote')}</span>
          </>
        }
      />

      <div className="flex flex-col gap-4 px-8 py-5">
        <div className="flex flex-wrap items-center gap-2.5">
          <div className="border-line-strong bg-background flex min-w-[280px] flex-1 items-center gap-2 border px-3 py-2">
            <Input
              aria-label={t('admin.images.search')}
              value={keyword}
              spellCheck={false}
              autoComplete="off"
              maxLength={KEYWORD_MAX}
              placeholder={t('admin.images.searchPlaceholder')}
              onChange={(event) => setKeyword(event.target.value)}
              className="h-auto rounded-none border-0 bg-transparent p-0 font-mono text-[13px] shadow-none focus-visible:ring-0 md:text-[13px] dark:bg-transparent"
            />
          </div>
          <select
            aria-label={t('admin.images.upstreamFilter')}
            value={upstreamID}
            onChange={(event) => setUpstreamID(Number(event.target.value))}
            className="border-line-strong bg-background text-foreground h-[38px] border px-3 font-mono text-[13px]"
          >
            <option value={0}>{t('admin.images.allUpstreams')}</option>
            {registryUpstreams.map((item) => (
              <option key={item.id} value={item.id}>
                {item.host}
              </option>
            ))}
          </select>
        </div>

        {target && (
          <div
            role="alertdialog"
            aria-label={t('admin.images.delete')}
            className="border-destructive flex flex-wrap items-center gap-3 border px-3 py-2"
          >
            <span className="text-foreground min-w-0 flex-1 text-[12.5px] break-all">
              {target.kind === 'image'
                ? t('admin.images.deleteImageQuestion', { name: target.name, count: target.count })
                : t('admin.images.deleteTagQuestion', { name: target.name, count: target.count })}
            </span>
            <button
              type="button"
              onClick={confirmPurge}
              disabled={action.pending}
              className="bg-destructive text-background px-3.5 py-1.5 text-xs disabled:opacity-45"
            >
              {t('admin.images.deleteConfirm')}
            </button>
            <button
              type="button"
              onClick={() => setTarget(null)}
              className="text-muted-foreground px-2 py-1.5 text-xs"
            >
              {t('admin.images.deleteCancel')}
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

        <Table aria-label={t('admin.images.title')} className="min-w-[820px] table-fixed text-left">
          <thead>
            <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
              <th scope="col" className="py-2 font-normal">
                {t('admin.images.columns.name')}
              </th>
              <th scope="col" className="w-[90px] py-2 font-normal">
                {t('admin.images.columns.tagCount')}
              </th>
              <th scope="col" className="w-[100px] py-2 font-normal">
                {t('admin.images.columns.size')}
              </th>
              <th scope="col" className="w-[90px] py-2 font-normal">
                {t('admin.images.columns.hits')}
              </th>
              <th scope="col" className="w-[120px] py-2 font-normal">
                {t('admin.images.columns.lastAccess')}
              </th>
              <th scope="col" className="w-[70px] py-2 font-normal">
                {t('admin.images.columns.operations')}
              </th>
            </tr>
          </thead>
          <tbody>
            {images.data.map((item) => {
              const key = rowKey(item.upstream_id, item.repository)
              const expanded = isExpanded(item)
              const rowTags = item.tags.length > 0 ? item.tags : tags[key]
              return (
                <Fragment key={key}>
                  <ImageRow
                    item={item}
                    expanded={expanded}
                    onToggle={() => toggleRow(item)}
                    onDelete={() => askPurgeImage(item)}
                  />
                  {expanded &&
                    rowTags?.map((tag) => (
                      <TagRow
                        key={tag.reference}
                        tag={tag}
                        onDelete={() => askPurgeTag(item, tag)}
                      />
                    ))}
                </Fragment>
              )
            })}
          </tbody>
        </Table>
        {images.data.length === 0 && !images.failed && (
          <p className="text-muted-foreground text-[13px]">{t('admin.images.empty')}</p>
        )}
        {images.hasMore && (
          <button
            type="button"
            onClick={() => images.loadMore()}
            className="text-primary self-start text-[12.5px]"
          >
            {t('admin.images.loadMore')}
          </button>
        )}
        <p className="text-ink-3 text-[11.5px]">{t('admin.images.sizeNote')}</p>
      </div>
    </>
  )
}

function ImageRow({
  item,
  expanded,
  onToggle,
  onDelete,
}: {
  item: CacheImageItem
  expanded: boolean
  onToggle: () => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  const name = `${item.host}/${item.repository}`
  const size = formatBytes(item.size)
  return (
    <tr className="border-border border-b">
      <td className="min-w-0 py-2.5">
        <div className="flex min-w-0 items-center gap-1.5">
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={t(expanded ? 'admin.images.collapse' : 'admin.images.expand', { name })}
            className="text-ink-3 hover:text-foreground shrink-0"
          >
            {expanded ? (
              <ChevronDown className="size-3.5" aria-hidden />
            ) : (
              <ChevronRight className="size-3.5" aria-hidden />
            )}
          </button>
          <Container className="text-muted-foreground size-3.5 shrink-0" aria-hidden />
          <span className="text-foreground min-w-0 truncate font-mono text-[13px]" title={name}>
            {name}
          </span>
        </div>
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {formatCount(item.tag_count)}
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {size.value} {size.unit}
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {formatCount(item.hit_count)}
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">
        <Stamp seconds={item.last_access_at} />
      </td>
      <td className="py-2.5">
        <button
          type="button"
          onClick={onDelete}
          className="text-muted-foreground hover:text-destructive text-[12.5px]"
        >
          {t('admin.images.delete')}
        </button>
      </td>
    </tr>
  )
}

function TagRow({ tag, onDelete }: { tag: CacheImageTag; onDelete: () => void }) {
  const { t } = useTranslation()
  const Icon = tag.by_digest ? Hash : TagIcon
  const name = tag.by_digest ? shortDigest(tag.reference) : tag.reference
  return (
    <tr className="border-border border-b">
      <td className="min-w-0 py-2.5">
        <div className="flex min-w-0 items-center gap-1.5" style={{ paddingLeft: INDENT_PX + 28 }}>
          <Icon className="text-ink-3 size-3.5 shrink-0" aria-hidden />
          <span className="text-foreground shrink-0 font-mono text-[13px]">{name}</span>
          {!tag.by_digest && tag.digest && (
            <span className="text-ink-3 shrink-0 font-mono text-[11px]">
              {shortDigest(tag.digest)}
            </span>
          )}
          {tag.variants > 1 && (
            <span className="text-muted-foreground border-line-strong shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.images.variants', { count: tag.variants })}
            </span>
          )}
          {tag.pinned && (
            <span className="text-primary border-primary shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.cache.pinned')}
            </span>
          )}
          {tag.expired && (
            <span className="text-warn border-warn shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.images.expired')}
            </span>
          )}
        </div>
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-[13px]">{ABSENT}</td>
      <td className="text-ink-3 py-2.5 font-mono text-[13px]">{ABSENT}</td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {formatCount(tag.hit_count)}
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">
        <Stamp seconds={tag.last_access_at} />
      </td>
      <td className="py-2.5">
        <button
          type="button"
          onClick={onDelete}
          className="text-muted-foreground hover:text-destructive text-[12.5px]"
        >
          {t('admin.images.delete')}
        </button>
      </td>
    </tr>
  )
}
