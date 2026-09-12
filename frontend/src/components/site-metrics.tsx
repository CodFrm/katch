import { useTranslation } from 'react-i18next'

import type { Overview } from '@/lib/api'
import { formatBytes, formatPercent } from '@/lib/format'
import { dailyHitRates, hitRate, savedBytesToday } from '@/lib/stats'
import { cn } from '@/lib/utils'

/** 柱子最矮也要看得见，否则「这一天没人来」会看起来像图画坏了。 */
const MIN_BAR = 6

function Metric({
  label,
  value,
  unit,
  className,
}: {
  label: string
  value: string
  unit: string
  className?: string
}) {
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      <span className="text-ink-3 text-[11px] tracking-[0.13em]">{label}</span>
      <div className="flex items-end gap-[5px]">
        <span className="text-foreground font-mono text-[34px] leading-none tracking-[-0.03em]">
          {value}
        </span>
        <span className="text-ink-3 pb-0.5 font-mono text-[13px]">{unit}</span>
      </div>
    </div>
  )
}

/**
 * 侧栏：命中率、缓存量、今天省下的回源流量，以及近 14 天的命中趋势。
 *
 * 三个数都是从公开总览接口的计数推出来的（见 lib/stats），后端不直接给比值。
 */
export function SiteMetrics({ overview }: { overview: Overview }) {
  const { t } = useTranslation()
  const cached = formatBytes(overview.cache_bytes)
  const saved = formatBytes(savedBytesToday(overview))
  const rates = dailyHitRates(overview)
  const peak = Math.max(...rates.map((point) => point.rate), 0)

  return (
    <aside className="flex w-[260px] shrink-0 flex-col">
      <Metric label={t('metrics.hitRate')} value={formatPercent(hitRate(overview))} unit="%" />
      <Metric
        label={t('metrics.cached')}
        value={cached.value}
        unit={cached.unit}
        className="border-border mt-[18px] border-t pt-[18px]"
      />
      <Metric
        label={t('metrics.savedToday')}
        value={saved.value}
        unit={saved.unit}
        className="border-border mt-[18px] border-t pt-[18px]"
      />
      <div className="border-border mt-[18px] flex flex-col gap-2.5 border-t pt-[18px]">
        <span className="text-ink-3 text-[11px] tracking-[0.13em]">{t('metrics.trend')}</span>
        <div
          role="img"
          aria-label={t('metrics.trendLabel')}
          className="flex h-14 items-end gap-[5px]"
        >
          {rates.map((point, index) => (
            <div
              key={point.day}
              data-slot="trend-bar"
              className={cn(
                'min-h-[2px] flex-1',
                index === rates.length - 1 ? 'bg-primary' : 'bg-line-strong'
              )}
              style={{
                height: `${peak > 0 ? Math.max(MIN_BAR, (point.rate / peak) * 100) : MIN_BAR}%`,
              }}
            />
          ))}
        </div>
      </div>
    </aside>
  )
}
