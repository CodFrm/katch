import { useTranslation } from 'react-i18next'

import { EventStream } from '@/components/admin/event-stream'
import { HealthMatrix } from '@/components/admin/health-matrix'
import { MetricBar, RangeTabs, type Metric } from '@/components/admin/metric-bar'
import { ScreenHeader } from '@/components/admin/admin-shell'
import { useOverviewExtras } from '@/hooks/use-admin-data'
import type { StatRange, UpstreamStatItem } from '@/lib/api'
import { formatBytes, formatCount, formatPercent } from '@/lib/format'
import {
  aggregateUpstreams,
  errorRate,
  hitRate,
  savedBytes,
  topOriginErrorUpstream,
} from '@/lib/stats'

/**
 * 概览：全局指标条 + 上游健康矩阵 + 事件流。
 *
 * 指标条的四个计数是按上游那份统计的合计，和矩阵读的是同一份数据——各问一次只会
 * 让同一屏上的数来自两个时刻，然后对不上。缓存占用来自站点总览，它是全站量。
 */
export function OverviewScreen({
  adminKey,
  range,
  onRangeChange,
  stats,
  onUnauthorized,
}: {
  adminKey: string
  range: StatRange
  onRangeChange: (range: StatRange) => void
  stats: UpstreamStatItem[]
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const { events, cacheBytes, series } = useOverviewExtras(adminKey, range, stats, onUnauthorized)
  const total = aggregateUpstreams(stats)
  const saved = formatBytes(savedBytes(total))
  const cached = cacheBytes === null ? null : formatBytes(cacheBytes)
  const worst = topOriginErrorUpstream(stats)
  const degraded = stats.filter((item) => item.degraded).length

  const metrics: Metric[] = [
    { label: t('admin.metrics.requests'), value: formatCount(total.requests) },
    {
      label: t('admin.metrics.overallHitRate'),
      value: formatPercent(hitRate(total)),
      unit: '%',
      emphasis: true,
    },
    { label: t('admin.metrics.saved'), value: saved.value, unit: saved.unit },
    ...(cached
      ? [{ label: t('admin.metrics.cached'), value: cached.value, unit: cached.unit }]
      : []),
    {
      label: t('admin.metrics.errorRate'),
      value: formatPercent(errorRate(total)),
      unit: '%',
      note: worst ? t('admin.metrics.errorFrom', { host: worst.host }) : undefined,
    },
  ]

  return (
    <>
      <ScreenHeader
        title={t('admin.overview.title')}
        subtitle={
          <>
            <span className="text-muted-foreground">
              {t('admin.overview.upstreamCount', { total: stats.length })}
            </span>
            <span className="text-ink-3">·</span>
            <span className="text-muted-foreground">
              {t('admin.overview.health', { normal: stats.length - degraded, degraded })}
            </span>
          </>
        }
        actions={<RangeTabs value={range} onChange={onRangeChange} />}
      />
      <MetricBar label={t('admin.metrics.globalLabel')} metrics={metrics} />
      <div className="flex flex-col gap-8 px-8 py-7">
        <HealthMatrix items={stats} series={series} />
        <EventStream events={events} />
      </div>
    </>
  )
}
