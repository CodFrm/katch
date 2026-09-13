import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { fetchRecentRequests, type RecentRequestItem } from '@/lib/api'
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
 * 数据来自结构化日志的有界尾部，不是一张每请求的表——决策 16 否掉了每请求写库
 * （那会把拉取热路径拖进事务），而分钟级的 rollup 答得了「这段时间总共怎么样」，
 * 答不了「刚刚发生了什么」。堆叠图和占比条说的是前一个问题，这张表说的是后一个。
 *
 * **读不到就整块消失**：日志没开落盘、文件刚被轮转走、够不到后端，后端都给一个
 * 空列表或一次失败，这里一律什么都不渲染。排障的辅助块消失，好过在管理界面上挂
 * 一句「打不开文件」——那句话既没有出口，也解释不了任何事。
 *
 * 取数写在这里而不是抽成公共钩子：只有这一块面板读这个端点，而它和「读不到就
 * 不渲染」是同一个决定的两半，分开放会让下一个人以为空列表也要画个空表头。
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

  if (list.length === 0) {
    return null
  }

  return (
    <section className="flex flex-col gap-3.5">
      <h2 className="text-foreground text-[12.5px] font-semibold">
        {t('admin.upstream.recent.title')}
      </h2>
      <table aria-label={t('admin.upstream.recent.title')} className="w-full text-left">
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
            // 同一秒里的两次拉取在日志里是两行，时刻做不了键——它本来就不唯一。
            <RequestRow key={`${item.at}-${index}`} item={item} />
          ))}
        </tbody>
      </table>
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
      <td className="text-foreground py-2.5 font-mono text-[12.5px]">{item.object}</td>
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

/**
 * 一个上游最近的几次拉取，读不到时是空数组。
 *
 * 「读不到」不再细分：没开日志落盘、文件被轮转走、够不到后端，对这块面板是同一
 * 件事——这次没有内容。只有 401 单独交回去，那是密钥的事，不是这块面板的事。
 */
function useRecentRequests(
  adminKey: string,
  upstreamID: number,
  onUnauthorized: () => void
): RecentRequestItem[] {
  const [loaded, setLoaded] = useState<{ id: number; list: RecentRequestItem[] } | null>(null)
  // 回调的身份每次渲染都可能变，跟着它进依赖数组就成了一个永不停的取数循环。
  const reject = useRef(onUnauthorized)
  useEffect(() => {
    reject.current = onUnauthorized
  }, [onUnauthorized])

  useEffect(() => {
    if (!Number.isFinite(upstreamID) || upstreamID <= 0) {
      return
    }
    const controller = new AbortController()
    void fetchRecentRequests(adminKey, upstreamID, controller.signal).then((result) => {
      if (controller.signal.aborted) {
        return
      }
      if (!result.ok) {
        if (result.reason === 'unauthorized') {
          reject.current()
        }
        return
      }
      setLoaded({ id: upstreamID, list: result.data.list ?? [] })
    })
    return () => controller.abort()
  }, [adminKey, upstreamID])

  // 上一个上游的行不许留在这一个的表上：手里那份数据属于别人。
  return loaded && loaded.id === upstreamID ? loaded.list : []
}
