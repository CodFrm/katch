import { useTranslation } from 'react-i18next'

import type { StatRange } from '@/lib/api'
import { cn } from '@/lib/utils'

export interface Metric {
  /** 指标名，已经翻译过的短语。 */
  label: string
  /** 机器产出的数，等宽。 */
  value: string
  /** 单位，没有单位的计数留空。 */
  unit?: string
  /** 底下那行注，没有可说的就不给——凑一句话比空着更糟。 */
  note?: string
  /** 主指标用强调色，其余用正文色。 */
  emphasis?: boolean
}

/**
 * 指标条：一排等宽的数，概览与上游详情共用。
 *
 * 只渲染真的有数据的指标：一个写着 0 的「P95 延迟」比没有这张卡更误导人。
 */
export function MetricBar({ label, metrics }: { label: string; metrics: Metric[] }) {
  return (
    <section
      role="group"
      aria-label={label}
      className="border-border flex flex-wrap border-b md:flex-nowrap"
    >
      {metrics.map((metric) => (
        <div
          key={metric.label}
          className="border-border flex min-w-[180px] flex-1 flex-col gap-2 border-r px-8 py-5 last:border-r-0"
        >
          <span className="text-ink-3 text-[11px] tracking-[0.12em]">{metric.label}</span>
          <div className="flex items-end gap-1">
            <span
              className={cn(
                'font-mono text-[28px] leading-none tracking-[-0.035em]',
                metric.emphasis ? 'text-primary' : 'text-foreground'
              )}
            >
              {metric.value}
            </span>
            {metric.unit && (
              <span className="text-ink-3 pb-0.5 font-mono text-xs">{metric.unit}</span>
            )}
          </div>
          {metric.note && <span className="text-ink-3 text-[11.5px]">{metric.note}</span>}
        </div>
      ))}
    </section>
  )
}

/** 区间切换：换的只是「看多久」，桶宽和口径都不跟着变。 */
export function RangeTabs({
  value,
  onChange,
}: {
  value: StatRange
  onChange: (range: StatRange) => void
}) {
  const { t } = useTranslation()
  const ranges: StatRange[] = ['24h', '7d', '30d']
  return (
    <div className="border-line-strong flex shrink-0 border">
      {ranges.map((range) => (
        <button
          key={range}
          type="button"
          onClick={() => onChange(range)}
          className={cn(
            'px-3.5 py-1.5 text-xs',
            range === value
              ? 'bg-foreground text-background'
              : 'text-muted-foreground hover:text-foreground'
          )}
        >
          {t(`admin.range.${range}`)}
        </button>
      ))}
    </div>
  )
}
