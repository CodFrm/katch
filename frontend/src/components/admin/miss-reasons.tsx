import { useTranslation } from 'react-i18next'

import type { UpstreamSeriesPoint } from '@/lib/api'
import { missReasonShares } from '@/lib/stats'
import { cn } from '@/lib/utils'

/**
 * 回源原因分解：这段区间里的回源，各是为什么发生的。
 *
 * 挨着 24 小时堆叠图放，读的也是同一份序列——堆叠图说的是「有多少请求最后还是
 * 去了上游」，这里回答紧接着的下一个问题：为什么去。两张图要是各打一次接口，
 * 同一屏上的两个数就会来自两个时刻。
 *
 * 「首次拉取」用强调色，其余三项用中性色：缓存还没热起来是一台新机器的正常
 * 状态，而 TTL、淘汰、内容变更多起来才是要动参数的信号，它们之间的对比比
 * 各给一种颜色更有用。
 */
export function MissReasons({ points }: { points: UpstreamSeriesPoint[] }) {
  const { t } = useTranslation()
  const shares = missReasonShares(points)

  return (
    <section
      role="group"
      aria-label={t('admin.upstream.missReasons.title')}
      className="border-border flex w-[296px] shrink-0 flex-col gap-4 border-l pl-[30px]"
    >
      <h2 className="text-foreground text-[12.5px] font-semibold">
        {t('admin.upstream.missReasons.title')}
      </h2>
      {shares.length === 0 ? (
        // 四条 0% 的条和「这段时间全是命中」长得一模一样，而后者是好消息。
        <p className="text-ink-3 text-[12.5px]">{t('admin.upstream.missReasons.empty')}</p>
      ) : (
        <ul className="flex flex-col gap-3.5">
          {shares.map((share) => (
            <li key={share.reason} data-slot="miss-reason" className="flex flex-col gap-1.5">
              <div className="flex items-baseline justify-between">
                <span data-slot="miss-label" className="text-muted-foreground text-xs">
                  {t(`admin.upstream.missReasons.reason.${share.reason}`)}
                </span>
                {/* 占比是机器产出的数，等宽。 */}
                <span data-slot="miss-share" className="text-foreground font-mono text-xs">
                  {t('admin.upstream.missReasons.percent', { percent: share.percent })}
                </span>
              </div>
              <div className="bg-muted h-1 w-full">
                <div
                  data-slot="miss-bar"
                  className={cn('h-1', share.reason === 'first' ? 'bg-primary' : 'bg-line-strong')}
                  style={{ width: `${share.percent}%` }}
                />
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
