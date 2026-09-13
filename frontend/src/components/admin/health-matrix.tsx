import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'

import type { UpstreamSeriesPoint, UpstreamStatItem } from '@/lib/api'
import { formatCount, formatPercent } from '@/lib/format'
import { hitRate, miniBars } from '@/lib/stats'
import { cn } from '@/lib/utils'

/** 迷你柱看最近 12 小时。 */
const MINI_HOURS = 12

/** 百分号是排版的一部分而不是文案：它在两种语言里都一样，翻译它没有意义。 */
const PERCENT = '%'

/** 柱子最矮也要看得见，否则「这个小时没人来」会看起来像图画坏了。 */
const MIN_BAR = 8

/**
 * 上游健康矩阵：一个上游一块，命中率 + 状态 + 近 12 小时请求量。
 *
 * 后端把没有流量的上游也列出来，所以刚加的上游在这里是一排零柱，而不是凭空消失。
 * 整块是链接：运维的下一个动作几乎总是「点进去看这台怎么了」。
 */
export function HealthMatrix({
  items,
  series,
}: {
  items: UpstreamStatItem[]
  series: Record<number, UpstreamSeriesPoint[]>
}) {
  const { t } = useTranslation()

  return (
    <section className="flex flex-col gap-3">
      <header className="flex items-baseline justify-between">
        <h2 className="text-foreground text-[12.5px] font-semibold">{t('admin.matrix.title')}</h2>
        <span className="text-ink-3 text-[11.5px]">{t('admin.matrix.hint')}</span>
      </header>
      <ul
        aria-label={t('admin.matrix.title')}
        className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4"
      >
        {items.map((item) => {
          const bars = miniBars(series[item.upstream_id] ?? [], MINI_HOURS)
          return (
            <li key={item.upstream_id}>
              <Link
                to={`/admin/upstreams/${item.upstream_id}`}
                className={cn(
                  'border-border hover:border-line-strong flex flex-col gap-3 border px-4 py-3.5',
                  item.degraded ? 'bg-muted' : 'bg-background'
                )}
              >
                <span className="flex items-center gap-2">
                  <span
                    aria-hidden
                    className={cn('size-[5px] rounded-full', item.degraded ? 'bg-warn' : 'bg-ok')}
                  />
                  <span className="text-foreground font-mono text-xs">{item.host}</span>
                  {item.degraded && (
                    <span className="text-warn text-[10.5px] tracking-[0.05em]">
                      {t('status.degraded')}
                    </span>
                  )}
                </span>
                <span className="flex items-end gap-1">
                  <span className="text-foreground font-mono text-[21px] leading-none tracking-[-0.03em]">
                    {formatPercent(hitRate(item))}
                  </span>
                  <span className="text-ink-3 pb-0.5 font-mono text-[11px]">{PERCENT}</span>
                  <span className="flex-1" />
                  <span className="text-ink-3 pb-0.5 font-mono text-[11px]">
                    {formatCount(item.requests)}
                  </span>
                </span>
                <span className="flex h-[26px] items-end gap-[3px]">
                  {bars.map((height, index) => (
                    <span
                      key={index}
                      data-slot="mini-bar"
                      className={cn(
                        'flex-1',
                        index === bars.length - 1 ? 'bg-line-strong' : 'bg-border'
                      )}
                      style={{ height: `${Math.max(MIN_BAR, height * 100)}%` }}
                    />
                  ))}
                </span>
              </Link>
            </li>
          )
        })}
      </ul>
    </section>
  )
}
