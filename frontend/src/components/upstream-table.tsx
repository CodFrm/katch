import { useTranslation } from 'react-i18next'

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import type { UpstreamItem, UpstreamProtocol } from '@/lib/api'
import { formatBytes, formatPercent } from '@/lib/format'
import { cn } from '@/lib/utils'

/**
 * 协议集合在界面上是一行标签，用间隔点连起来。
 *
 * 放在组件外面而不是写在 JSX 里：那个分隔符是排版符号而不是文案，写在 JSX 里
 * 会被 i18next/no-literal-string 当成漏翻的界面文字拦下来。
 */
function protocolLabels(protocols: UpstreamProtocol[], t: (key: string) => string): string {
  return protocols.map((protocol) => t(`protocol.${protocol}`)).join(' · ')
}

/** 表格里值和单位挨在一起，不像侧栏那样分开排。 */
function formatSize(bytes: number): string {
  const { value, unit } = formatBytes(bytes)
  return `${value} ${unit}`
}

/**
 * 页脚那张「支持的上游」表。
 *
 * 「支不支持我要的东西」不用点进去就能答（spec 前台一节），所以它整幅铺在页脚，
 * 而不是藏在一个入口后面。列表只含启用中的上游——停用与不存在在拉取路径上是
 * 同一件事，这里也必须是同一件事。
 */
export function UpstreamTable({ upstreams }: { upstreams: UpstreamItem[] }) {
  const { t } = useTranslation()
  const columns = [
    { key: 'host', width: 'w-[280px]' },
    { key: 'protocols', width: 'w-[150px]' },
    { key: 'hitRate', width: 'w-[110px]' },
    { key: 'cached', width: 'w-[110px]' },
    { key: 'status', width: '' },
  ] as const

  return (
    <section className="bg-muted border-border border-t px-12 pt-[22px] pb-7">
      <div className="flex items-center justify-between pb-3.5">
        <h2 className="text-foreground text-[13px] font-semibold">{t('upstreams.title')}</h2>
        <span className="text-muted-foreground text-xs">
          {t('upstreams.count', { count: upstreams.length })}
        </span>
      </div>
      <Table className="text-[13px]">
        <TableHeader>
          <TableRow className="border-border hover:bg-transparent">
            {columns.map((column) => (
              <TableHead
                key={column.key}
                className={cn(
                  'text-ink-3 h-auto px-0 pb-2.5 text-[11px] font-normal tracking-[0.1em]',
                  column.width
                )}
              >
                {t(`upstreams.${column.key}`)}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {upstreams.map((upstream) => (
            <TableRow key={upstream.host} className="border-border hover:bg-transparent">
              <TableCell className="text-foreground px-0 py-2.5 font-mono">
                {upstream.host}
              </TableCell>
              <TableCell className="text-muted-foreground px-0 py-2.5">
                {protocolLabels(upstream.protocols, t)}
              </TableCell>
              <TableCell className="text-foreground px-0 py-2.5 font-mono">
                {`${formatPercent(upstream.hit_rate)}%`}
              </TableCell>
              <TableCell className="text-muted-foreground px-0 py-2.5 font-mono">
                {formatSize(upstream.cache_bytes)}
              </TableCell>
              <TableCell className="px-0 py-2.5">
                <span
                  className={cn(
                    'flex items-center gap-[7px]',
                    upstream.status === 'normal' ? 'text-ok' : 'text-warn'
                  )}
                >
                  <span
                    aria-hidden="true"
                    className={cn(
                      'size-1.5 rounded-full',
                      upstream.status === 'normal' ? 'bg-ok' : 'bg-warn'
                    )}
                  />
                  {t(`status.${upstream.status}`)}
                </span>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </section>
  )
}
