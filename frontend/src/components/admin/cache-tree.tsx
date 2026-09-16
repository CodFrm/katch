import { ArrowRight, ChevronDown, ChevronRight, File, Folder, FolderOpen } from 'lucide-react'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import type { CacheTreeObjectItem } from '@/lib/api'
import { formatBytes, formatCount, formatStamp } from '@/lib/format'

/** 目录树一共几列：勾选、名称、对象数、大小、命中、最后访问、操作。 */
export const TREE_COLUMNS = 7

/** 目录行没有命中、对象行没有对象数：那一格写这个，不留白。容器镜像页同用。 */
export const ABSENT = '—'

/** 目录名后面那一撇，表示它是目录。 */
const DIR_SUFFIX = '/'

/** 每深一层缩进多少像素。容器镜像页同用。 */
export const INDENT_PX = 18

/**
 * 摘要在表里只留一小截：整串 sha256 占满一行，而运维要的只是「这两条是不是同一份」。
 * 算法前缀换成 @，那是排版，不是文案。
 */
export function shortDigest(digest: string): string {
  return digest.replace('sha256:', '@').slice(0, 10)
}

/** 把名字里与搜索词（不区分大小写）相同的片段包成 mark。没有搜索词时原样给回。 */
export function Highlight({ text, keyword }: { text: string; keyword: string }) {
  if (keyword === '') {
    return <>{text}</>
  }
  const lower = text.toLowerCase()
  const needle = keyword.toLowerCase()
  const parts: ReactNode[] = []
  let from = 0
  for (let at = lower.indexOf(needle); at >= 0; at = lower.indexOf(needle, from)) {
    if (at > from) {
      parts.push(text.slice(from, at))
    }
    parts.push(
      <mark key={at} className="bg-signal-soft text-primary">
        {text.slice(at, at + needle.length)}
      </mark>
    )
    from = at + needle.length
  }
  if (from < text.length) {
    parts.push(text.slice(from))
  }
  return <>{parts}</>
}

/** 最后访问时刻：今天只写时刻，昨天写「昨天」，更早写日期。 */
export function Stamp({ seconds }: { seconds: number }) {
  const { t } = useTranslation()
  if (seconds <= 0) {
    return <>{ABSENT}</>
  }
  const stamp = formatStamp(seconds)
  if (stamp.day === 'today') {
    return <>{stamp.time}</>
  }
  if (stamp.day === 'yesterday') {
    return <>{t('admin.events.yesterday', { time: stamp.time })}</>
  }
  return <>{`${stamp.date} ${stamp.time}`}</>
}

function Bytes({ size }: { size: number }) {
  const formatted = formatBytes(size)
  return <>{`${formatted.value} ${formatted.unit}`}</>
}

function PurgeButton({ onClick }: { onClick: () => void }) {
  const { t } = useTranslation()
  return (
    <button
      type="button"
      onClick={onClick}
      className="text-muted-foreground hover:text-destructive text-[12.5px]"
    >
      {t('admin.cache.purge')}
    </button>
  )
}

/**
 * 目录行：三角就地展开，点名字或箭头进入。数字是其下（递归）全部对象的合计；
 * 搜索时给的是匹配部分的合计，并标出匹配数。目录行不能勾选——批量操作只对对象。
 */
export function DirRow({
  name,
  path,
  depth,
  expanded,
  count,
  size,
  lastAccessAt,
  matched,
  keyword,
  onToggle,
  onEnter,
  onPurge,
}: {
  name: string
  path: string
  depth: number
  expanded: boolean
  count: number
  size: number
  lastAccessAt: number
  /** 搜索时其下匹配到的对象数；浏览时不给。 */
  matched?: number
  keyword: string
  onToggle: () => void
  onEnter: () => void
  onPurge: () => void
}) {
  const { t } = useTranslation()
  const FolderIcon = expanded ? FolderOpen : Folder
  return (
    <tr className="border-border border-b">
      <td className="py-2.5" />
      <td className="min-w-0 py-2.5">
        <div
          className="flex min-w-0 items-center gap-1.5"
          style={{ paddingLeft: depth * INDENT_PX }}
        >
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={t(expanded ? 'admin.cache.tree.collapse' : 'admin.cache.tree.expand', {
              name,
            })}
            className="text-ink-3 hover:text-foreground shrink-0"
          >
            {expanded ? (
              <ChevronDown className="size-3.5" aria-hidden />
            ) : (
              <ChevronRight className="size-3.5" aria-hidden />
            )}
          </button>
          <FolderIcon className="text-muted-foreground size-3.5 shrink-0" aria-hidden />
          <button
            type="button"
            onClick={onEnter}
            title={path}
            className="text-foreground hover:text-primary min-w-0 truncate text-left font-mono text-[13px]"
          >
            <Highlight text={name} keyword={keyword} />
            {DIR_SUFFIX}
          </button>
          {matched !== undefined && (
            <span className="text-primary border-primary shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.cache.tree.matched', { count: matched })}
            </span>
          )}
          <button
            type="button"
            onClick={onEnter}
            aria-label={t('admin.cache.tree.enter', { name })}
            className="text-ink-3 hover:text-primary shrink-0"
          >
            <ArrowRight className="size-3.5" aria-hidden />
          </button>
        </div>
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">{formatCount(count)}</td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        <Bytes size={size} />
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-[13px]">{ABSENT}</td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">
        <Stamp seconds={lastAccessAt} />
      </td>
      <td className="py-2.5">
        <PurgeButton onClick={onPurge} />
      </td>
    </tr>
  )
}

/** 对象行：叶子名（去掉变体段）、短摘要、固定与变体标记；清除走单条清除，不要确认。 */
export function ObjectRow({
  name,
  path,
  variant,
  object,
  depth,
  keyword,
  checked,
  onToggle,
  onPurge,
}: {
  name: string
  path: string
  variant: boolean
  object: CacheTreeObjectItem
  depth: number
  keyword: string
  checked: boolean
  onToggle: () => void
  onPurge: () => void
}) {
  const { t } = useTranslation()
  return (
    <tr className="border-border border-b">
      <td className="py-2.5">
        <input
          type="checkbox"
          checked={checked}
          onChange={onToggle}
          aria-label={t('admin.cache.selectObject', { key: path })}
          className="accent-primary size-3"
        />
      </td>
      <td className="min-w-0 py-2.5">
        <div
          // 三角的位置空出来，文件图标才和同层目录的文件夹图标对齐。
          className="flex min-w-0 items-center gap-1.5"
          style={{ paddingLeft: depth * INDENT_PX + INDENT_PX + 2 }}
        >
          <File className="text-ink-3 size-3.5 shrink-0" aria-hidden />
          <span
            className="text-foreground block min-w-0 flex-1 truncate font-mono text-[13px]"
            title={path}
          >
            <Highlight text={name} keyword={keyword} />
          </span>
          {object.digest && (
            <span className="text-ink-3 shrink-0 font-mono text-[11px]">
              {shortDigest(object.digest)}
            </span>
          )}
          {variant && (
            <span className="text-muted-foreground border-line-strong shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.cache.tree.variant')}
            </span>
          )}
          {object.pinned && (
            <span className="text-primary border-primary shrink-0 border px-1.5 text-[10.5px]">
              {t('admin.cache.pinned')}
            </span>
          )}
        </div>
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-[13px]">{ABSENT}</td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        <Bytes size={object.size} />
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {formatCount(object.hit_count)}
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">
        <Stamp seconds={object.last_access_at} />
      </td>
      <td className="py-2.5">
        <PurgeButton onClick={onPurge} />
      </td>
    </tr>
  )
}

/** 一层末尾的「加载更多」：一层最多给一批，剩下的点了再接上。 */
export function LoadMoreRow({ depth, onClick }: { depth: number; onClick: () => void }) {
  const { t } = useTranslation()
  return (
    <tr className="border-border border-b">
      <td colSpan={TREE_COLUMNS} className="py-2">
        <div style={{ paddingLeft: depth * INDENT_PX + 28 + INDENT_PX }}>
          <button type="button" onClick={onClick} className="text-primary text-[12.5px]">
            {t('admin.cache.tree.loadMore')}
          </button>
        </div>
      </td>
    </tr>
  )
}
