import { useTranslation } from 'react-i18next'

import { Table } from '@/components/ui/table'
import { useRecentRequests } from '@/hooks/use-admin-data'
import { type RecentRequestItem } from '@/lib/api'
import { formatBytes, formatClock, formatLatency } from '@/lib/format'

/** 没量到的那一格用破折号：它和 `0 B` / `0 ms` 是两句话。 */
const ABSENT = '—'

/**
 * 每种结果配一种颜色。取值与后端 katch_requests_total 的 result 同一套。
 *
 * 命中是好事（ok），回源是这台机器在干活（signal），拒绝和回源失败各占一头：
 * 前者是规则按预期挡住了，后者是上游那边出了问题。
 */
const RESULT_TONE: Record<RecentRequestItem['result'], string> = {
  hit: 'text-ok',
  miss: 'text-primary',
  denied: 'text-destructive',
  origin_error: 'text-warn',
}

/**
 * 某个上游最近的几次拉取。
 *
 * 数据来自 recent_request 这张每请求一行的明细表，不是日志文件（决策 2/11）：分钟级
 * 的 rollup 答「这段时间总共怎么样」（可加），这张表答「刚刚按什么顺序发生了什么」
 * （不可加，每请求一行）。堆叠图和占比条说的是前一个问题，这张表说的是后一个。
 *
 * **读不到就整块消失**：库里没有这个上游的行、库读不出来、够不到后端，还没问到——
 * 这些对这块面板是同一件事，一律什么都不渲染。排障的辅助块消失，好过在管理界面上
 * 挂一句读不到数据——那句话既没有出口，也解释不了任何事。
 *
 * 取数本身交给 use-admin-data 的 useRecentRequests：那套「id 不合法就不取、取数可
 * 取消、401 单独交回、切上游时上一个的数据不许留下」的机制，和按上游取时序的那个
 * 钩子是同一套，各写一份只会让两份分家。留在这里的只有上面那个呈现决定。
 */
export function RecentRequests({
  adminKey,
  upstreamID,
  onUnauthorized,
}: {
  adminKey: string
  upstreamID: number
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const list = useRecentRequests(adminKey, upstreamID, onUnauthorized)

  // null 是「还没问到」，空数组是「问到了但没有内容」——这块面板对两者是同一个答复。
  if (!list || list.length === 0) {
    return null
  }

  return (
    <section className="flex flex-col gap-3.5">
      <h2 className="text-foreground text-[12.5px] font-semibold">
        {t('admin.upstream.recent.title')}
      </h2>
      <Table
        aria-label={t('admin.upstream.recent.title')}
        className="min-w-[640px] table-fixed text-left"
      >
        <thead>
          <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
            <th scope="col" className="w-[84px] pb-2.5 font-normal">
              {t('admin.upstream.recent.columns.at')}
            </th>
            <th scope="col" className="pb-2.5 font-normal">
              {t('admin.upstream.recent.columns.object')}
            </th>
            <th scope="col" className="w-[96px] pb-2.5 font-normal">
              {t('admin.upstream.recent.columns.result')}
            </th>
            <th scope="col" className="w-[92px] pb-2.5 font-normal">
              {t('admin.upstream.recent.columns.bytes')}
            </th>
            <th scope="col" className="w-[70px] pb-2.5 font-normal">
              {t('admin.upstream.recent.columns.duration')}
            </th>
          </tr>
        </thead>
        <tbody>
          {list.map((item, index) => (
            // 同一秒里的两次拉取在库里是两行，时刻做不了键——它本来就不唯一。
            <RequestRow key={`${item.at}-${index}`} item={item} />
          ))}
        </tbody>
      </Table>
    </section>
  )
}

/** 一次拉取。机器产出或消费的一律等宽，结果那一格是人读的词，用无衬线。 */
function RequestRow({ item }: { item: RecentRequestItem }) {
  const { t } = useTranslation()
  const size = formatBytes(item.bytes)

  return (
    <tr data-slot="recent-request" className="border-border border-b">
      <td className="text-muted-foreground py-2.5 font-mono text-[12.5px]">
        {formatClock(item.at)}
      </td>
      <td className="text-foreground py-2.5 font-mono text-[12.5px]">
        <span className="block truncate pr-4" title={item.object}>
          {item.object}
        </span>
      </td>
      <td className={`py-2.5 text-[12.5px] ${RESULT_TONE[item.result] ?? 'text-muted-foreground'}`}>
        <span className="flex items-center gap-1.5">
          <span aria-hidden className="size-[5px] shrink-0 rounded-full bg-current" />
          {t(`admin.upstream.recent.result.${item.result}`)}
        </span>
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[12.5px]">
        {item.bytes > 0 ? `${size.value} ${size.unit}` : ABSENT}
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[12.5px]">
        {item.duration_ms > 0 ? formatLatency(item.duration_ms) : ABSENT}
      </td>
    </tr>
  )
}
