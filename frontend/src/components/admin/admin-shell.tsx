import { LayoutGrid, LogOut } from 'lucide-react'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink } from 'react-router-dom'

import type { UpstreamStatItem } from '@/lib/api'
import { formatPercent } from '@/lib/format'
import { hitRate } from '@/lib/stats'
import { cn } from '@/lib/utils'

/** 字标与当前域之间的分隔符，是排版而不是文案。 */
const SEPARATOR = '/'

/**
 * 后台外壳：左栏常驻，右侧换内容。
 *
 * 上游列表常驻在左栏，不做成一个要点进去的模块：运维的问题几乎都是「某个上游
 * 怎么了」，平级模块会逼人跳页拼信息。
 */
export function AdminShell({
  stats,
  onSignOut,
  children,
}: {
  stats: UpstreamStatItem[]
  onSignOut: () => void
  children: ReactNode
}) {
  const { t } = useTranslation()

  return (
    <div className="bg-background flex min-h-screen">
      <aside className="border-border flex w-[284px] shrink-0 flex-col border-r">
        <header className="border-border flex h-15 items-center gap-1.5 border-b px-5">
          <span className="text-foreground font-mono text-sm font-semibold">{t('app.name')}</span>
          <span className="text-ink-3 text-sm">{SEPARATOR}</span>
          <span className="text-muted-foreground text-[13px]">{t('nav.admin')}</span>
          <span className="flex-1" />
          <button
            type="button"
            onClick={onSignOut}
            aria-label={t('admin.signOut')}
            className="text-ink-3 hover:text-foreground"
          >
            <LogOut className="size-4" aria-hidden />
          </button>
        </header>

        <nav className="flex flex-col gap-0.5 px-2.5 py-4">
          <NavLink
            to="/admin"
            end
            className={({ isActive }) =>
              cn(
                'flex items-center gap-2.5 px-2.5 py-2 text-[13px]',
                isActive
                  ? 'bg-muted text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              )
            }
          >
            <LayoutGrid className="size-[15px]" aria-hidden />
            {t('admin.nav.overview')}
          </NavLink>
        </nav>

        <div className="flex min-w-0 flex-1 flex-col gap-1 px-2.5 pb-4">
          <div className="flex items-baseline gap-1.5 px-2.5 pb-1.5">
            <span className="text-ink-3 text-[11px] tracking-[0.12em]">
              {t('admin.nav.upstreams')}
            </span>
            <span className="text-ink-3 font-mono text-[11px]">{stats.length}</span>
          </div>
          {stats.map((item) => (
            <NavLink
              key={item.upstream_id}
              to={`/admin/upstreams/${item.upstream_id}`}
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-2 px-2.5 py-1.5',
                  isActive ? 'bg-muted' : 'hover:bg-muted/60'
                )
              }
            >
              <span
                aria-hidden
                className={cn('size-[5px] rounded-full', item.degraded ? 'bg-warn' : 'bg-ok')}
              />
              <span className="text-foreground truncate font-mono text-xs">{item.host}</span>
              <span className="flex-1" />
              <span className="text-ink-3 font-mono text-[11px]">
                {formatPercent(hitRate(item))}
              </span>
            </NavLink>
          ))}
        </div>
      </aside>

      <main className="flex min-w-0 flex-1 flex-col">{children}</main>
    </div>
  )
}

/** 两个屏幕共用的页头骨架：标题块在左，区间切换一类的操作在右。 */
export function ScreenHeader({
  title,
  mono,
  badges,
  subtitle,
  actions,
}: {
  title: string
  /** 标题是机器串（主机名）时用等宽，是人写的词（概览）时用无衬线。 */
  mono?: boolean
  badges?: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
}) {
  return (
    <header className="border-border flex items-center gap-5 border-b px-8 pt-6 pb-5">
      <div className="flex min-w-0 flex-col gap-[7px]">
        <div className="flex items-center gap-2.5">
          <h1
            className={cn(
              'text-foreground leading-none',
              mono
                ? 'font-mono text-[27px] tracking-[-0.03em]'
                : 'text-2xl font-semibold tracking-[-0.015em]'
            )}
          >
            {title}
          </h1>
          {badges}
        </div>
        {subtitle && <div className="flex items-center gap-2 text-xs">{subtitle}</div>}
      </div>
      <span className="flex-1" />
      {actions}
    </header>
  )
}
