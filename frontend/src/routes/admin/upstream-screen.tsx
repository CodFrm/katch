import { useTranslation } from 'react-i18next'
import { useParams } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { HourlyChart } from '@/components/admin/hourly-chart'
import { MetricBar, type Metric } from '@/components/admin/metric-bar'
import { useUpstreamSeries } from '@/hooks/use-admin-data'
import type { AdminUpstreamItem, StatRange, UpstreamStatItem } from '@/lib/api'
import { formatBytes, formatCount, formatPercent } from '@/lib/format'
import { hitRate, originRequests, savedBytes } from '@/lib/stats'

/**
 * 上游详情：这个上游的指标条与逐小时的命中/回源图。
 *
 * 主机名用等宽、回源地址用等宽，类型和状态用无衬线——机器产出或消费的一律等宽。
 */
export function UpstreamScreen({
  adminKey,
  range,
  stats,
  upstreams,
  onUnauthorized,
}: {
  adminKey: string
  range: StatRange
  stats: UpstreamStatItem[]
  upstreams: AdminUpstreamItem[]
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const { id } = useParams()
  const upstreamID = Number(id)
  const points = useUpstreamSeries(adminKey, upstreamID, range, onUnauthorized)
  const stat = stats.find((item) => item.upstream_id === upstreamID)
  const upstream = upstreams.find((item) => item.id === upstreamID)

  if (!stat && stats.length > 0) {
    // 列表已经回来了，里面却没有这个 id：链接过期或者被人手改了地址。
    return (
      <div className="px-8 py-7">
        <p className="text-muted-foreground text-[13px]">{t('admin.upstream.missing')}</p>
      </div>
    )
  }
  if (!stat) {
    return null
  }

  const served = formatBytes(stat.bytes_served)
  const saved = formatBytes(savedBytes(stat))
  const metrics: Metric[] = [
    {
      label: t('admin.metrics.hitRate'),
      value: formatPercent(hitRate(stat)),
      unit: '%',
      emphasis: true,
    },
    { label: t('admin.upstream.originRequests'), value: formatCount(originRequests(stat)) },
    { label: t('admin.upstream.served'), value: served.value, unit: served.unit },
    { label: t('admin.metrics.saved'), value: saved.value, unit: saved.unit },
  ]

  return (
    <>
      <ScreenHeader
        title={stat.host}
        mono
        badges={
          <>
            {upstream && (
              <span
                className={
                  upstream.enabled
                    ? 'bg-ok text-background px-[7px] py-[3px] text-[10.5px] tracking-[0.05em]'
                    : 'text-ink-3 border-border border px-[7px] py-[3px] text-[10.5px] tracking-[0.05em]'
                }
              >
                {t(upstream.enabled ? 'admin.upstream.enabled' : 'admin.upstream.disabled')}
              </span>
            )}
            {stat.degraded && (
              <span className="text-warn border-warn border px-[7px] py-[3px] text-[10.5px] tracking-[0.05em]">
                {t('status.degraded')}
              </span>
            )}
          </>
        }
        subtitle={
          upstream && (
            <>
              <span className="text-muted-foreground">{t(`kind.${upstream.kind}`)}</span>
              <span className="text-ink-3">·</span>
              <span className="text-muted-foreground font-mono">{upstream.origin}</span>
            </>
          )
        }
      />
      <MetricBar label={t('admin.upstream.metricsLabel')} metrics={metrics} />
      <div className="flex flex-col gap-8 px-8 py-7">
        {points && <HourlyChart points={points} range={range} />}
      </div>
    </>
  )
}
