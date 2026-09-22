import { useTranslation } from 'react-i18next'

import { Table } from '@/components/ui/table'
import type { EventItem } from '@/lib/api'
import { actorKey, describeEvent, type EventTone } from '@/lib/events'
import { formatStamp } from '@/lib/format'
import { cn } from '@/lib/utils'

/** 标签的语气对应哪套颜色。降级用警示、恢复用正常、回收与未知用弱化、变更用强调。 */
const TONE_CLASS: Record<EventTone, string> = {
  change: 'border-primary text-primary',
  degraded: 'border-warn text-warn',
  recovered: 'border-ok text-ok',
  reclaim: 'border-border text-ink-3',
  unknown: 'border-border text-ink-3',
}

/**
 * 事件流：自动告警与人为变更在同一条时间线上，带操作人。
 *
 * 每一行的措辞都是界面组织的：后端只给 kind / actor 两个枚举和结构化的 detail
 * （它刻意不给句子）。认不出来的 kind 也走翻译表里的兜底文案，绝不把枚举贴出来。
 */
export function EventStream({ events }: { events: EventItem[] }) {
  const { t } = useTranslation()

  return (
    <section className="flex flex-col gap-3">
      <header className="flex items-baseline justify-between">
        <h2 className="text-foreground text-[12.5px] font-semibold">{t('admin.events.title')}</h2>
      </header>
      {events.length === 0 ? (
        <p className="text-ink-3 text-[12.5px]">{t('admin.events.empty')}</p>
      ) : (
        <Table
          aria-label={t('admin.events.title')}
          className="min-w-[600px] table-fixed border-collapse"
        >
          <tbody>
            {events.map((event) => {
              const view = describeEvent(event)
              const stamp = formatStamp(event.createtime)
              const enums = Object.fromEntries(
                Object.entries(view.enums).map(([field, key]) => [field, t(key)])
              )
              const description = t(view.key, { ...view.values, ...enums })
              return (
                <tr key={event.id} className="border-border border-b">
                  <td className="text-ink-3 w-24 py-2 font-mono text-xs">
                    {stamp.day === 'today'
                      ? stamp.time
                      : stamp.day === 'yesterday'
                        ? t('admin.events.yesterday', { time: stamp.time })
                        : `${stamp.date} ${stamp.time}`}
                  </td>
                  <td className="w-[76px] py-2">
                    <span
                      className={cn(
                        'border px-[7px] py-[2px] text-[10.5px] tracking-[0.04em]',
                        TONE_CLASS[view.tone]
                      )}
                    >
                      {t(`admin.events.tone.${view.tone}`)}
                    </span>
                  </td>
                  <td className="text-foreground py-2 text-[12.5px]">
                    <span className="block truncate pr-4" title={description}>
                      {description}
                    </span>
                  </td>
                  <td className="text-ink-3 w-24 py-2 text-right text-xs">
                    {t(actorKey(event.actor))}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </Table>
      )}
    </section>
  )
}
