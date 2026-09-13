import { useTranslation } from 'react-i18next'

import type { StatRange, UpstreamSeriesPoint } from '@/lib/api'
import { stackedBars } from '@/lib/stats'

/** 柱子最矮也要看得见，否则「这个小时没人来」会看起来像图画坏了。 */
const MIN_BAR = 2

/**
 * 逐小时的命中/回源堆叠图。
 *
 * 回源那段叠在命中之上：一眼要看出的是「这个小时有多少请求最后还是去了上游」，
 * 把它放在顶上，多台上游并排看时那条锯齿线对得上。
 * 高度按区间峰值归一（见 lib/stats），所以忙闲看得出来。
 */
export function HourlyChart({
  points,
  range,
}: {
  points: UpstreamSeriesPoint[]
  range: StatRange
}) {
  const { t } = useTranslation()
  const bars = stackedBars(points)
  const windowLabel = t(`admin.range.${range}`)

  return (
    <section className="flex flex-1 flex-col gap-3">
      <header className="flex items-center justify-between">
        <h2 className="text-foreground text-[12.5px] font-semibold">
          {t('admin.upstream.chartTitle', { range: windowLabel })}
        </h2>
        <div className="flex items-center gap-4">
          <span className="flex items-center gap-1.5">
            <span aria-hidden className="bg-line-strong size-[9px]" />
            <span className="text-muted-foreground text-[11.5px]">{t('admin.upstream.hits')}</span>
          </span>
          <span className="flex items-center gap-1.5">
            <span aria-hidden className="bg-primary size-[9px]" />
            <span className="text-muted-foreground text-[11.5px]">
              {t('admin.upstream.origin')}
            </span>
          </span>
        </div>
      </header>
      <div
        role="img"
        aria-label={t('admin.upstream.chartLabel', { range: windowLabel })}
        className="flex h-[178px] items-end gap-1.5"
      >
        {bars.map((bar) => (
          <div
            key={bar.bucket}
            data-slot="series-bar"
            className="flex flex-1 flex-col justify-end gap-px"
            style={{ height: `${Math.max(MIN_BAR, bar.height * 100)}%` }}
          >
            <div
              data-slot="series-origin"
              className="bg-primary w-full shrink-0"
              style={{ height: `${bar.originShare * 100}%` }}
            />
            <div data-slot="series-hit" className="bg-line-strong w-full flex-1" />
          </div>
        ))}
      </div>
      <div className="border-border text-ink-3 flex justify-between border-t pt-2 font-mono text-[10.5px]">
        <span>{t('admin.upstream.axisStart', { range: windowLabel })}</span>
        <span>{t('admin.upstream.axisNow')}</span>
      </div>
    </section>
  )
}
