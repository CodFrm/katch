import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink, useNavigate, useParams } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { HourlyChart } from '@/components/admin/hourly-chart'
import { MetricBar, type Metric } from '@/components/admin/metric-bar'
import { MissReasons } from '@/components/admin/miss-reasons'
import { useAdminAction } from '@/hooks/use-admin-action'
import { useUpstreamSeries } from '@/hooks/use-admin-data'
import {
  purgeCache,
  saveUpstream,
  toUpstreamDraft,
  type AdminUpstreamItem,
  type StatRange,
  type UpstreamStatItem,
} from '@/lib/api'
import { formatBytes, formatCount, formatPercent } from '@/lib/format'
import { hitRate, originRequests, savedBytes } from '@/lib/stats'
import { RulesScreen } from '@/routes/admin/rules-screen'
import { cn } from '@/lib/utils'

/** 上游详情上的两个页签。日志那一类还没有后端端点，就不在这里画一个进不去的门。 */
type Tab = 'overview' | 'rules'

const TABS: Tab[] = ['overview', 'rules']

/**
 * 上游详情：这个上游的指标条与逐小时的命中/回源图，加上它自己的访问规则。
 *
 * 主机名用等宽、回源地址用等宽，类型和状态用无衬线——机器产出或消费的一律等宽。
 */
export function UpstreamScreen({
  adminKey,
  range,
  stats,
  upstreams,
  tab,
  onUnauthorized,
  onChange,
}: {
  adminKey: string
  range: StatRange
  stats: UpstreamStatItem[]
  upstreams: AdminUpstreamItem[]
  tab: Tab
  onUnauthorized: () => void
  /** 启停、编辑、改默认策略之后让外面那份上游数据跟上。 */
  onChange: () => void
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
        actions={
          upstream && (
            <UpstreamActions
              adminKey={adminKey}
              upstream={upstream}
              onUnauthorized={onUnauthorized}
              onChange={onChange}
            />
          )
        }
      />
      <nav className="border-border flex gap-10 border-b px-8">
        {TABS.map((item) => (
          <NavLink
            key={item}
            end={item === 'overview'}
            to={
              item === 'overview'
                ? `/admin/upstreams/${upstreamID}`
                : `/admin/upstreams/${upstreamID}/rules`
            }
            className={({ isActive }) =>
              cn(
                'border-b-2 py-3.5 text-[13px]',
                isActive
                  ? 'border-foreground text-foreground'
                  : 'text-muted-foreground hover:text-foreground border-transparent'
              )
            }
          >
            {t(`admin.upstream.tab.${item}`)}
          </NavLink>
        ))}
      </nav>

      {tab === 'overview' ? (
        <>
          <MetricBar label={t('admin.upstream.metricsLabel')} metrics={metrics} />
          <div className="flex flex-col gap-8 px-8 py-7">
            {points && (
              // 一排两块：左边「有多少请求去了上游」，右边「为什么去」。
              // 分两屏会逼人把两个数记在脑子里再比。
              <div className="flex gap-[30px]">
                <HourlyChart points={points} range={range} />
                <MissReasons points={points} />
              </div>
            )}
          </div>
        </>
      ) : (
        <RulesScreen
          adminKey={adminKey}
          upstream={upstream ?? null}
          onUnauthorized={onUnauthorized}
          onUpstreamChange={onChange}
        />
      )}
    </>
  )
}

/**
 * 详情头上的三个操作：暂停/恢复、清空这个上游的缓存、编辑登记信息。
 *
 * 停用等同于不在白名单里（既不回源也不回显），清缓存是不可逆的批量操作——所以
 * 清缓存要再确认一次，而停用不用：它随时能开回来。
 */
function UpstreamActions({
  adminKey,
  upstream,
  onUnauthorized,
  onChange,
}: {
  adminKey: string
  upstream: AdminUpstreamItem
  onUnauthorized: () => void
  onChange: () => void
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const action = useAdminAction(onUnauthorized)
  const [confirming, setConfirming] = useState(false)
  const [purged, setPurged] = useState<{ removed: number; skipped: number } | null>(null)

  function toggleEnabled() {
    // 整条写回去：保存是覆盖而不是打补丁。
    void action.run(
      () => saveUpstream(adminKey, { ...toUpstreamDraft(upstream), enabled: !upstream.enabled }),
      onChange
    )
  }

  function purge() {
    void action.run(
      () => purgeCache(adminKey, { upstreamID: upstream.id }),
      (result) => {
        setConfirming(false)
        setPurged(result)
      }
    )
  }

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={toggleEnabled}
          disabled={action.pending}
          className="border-line-strong text-foreground border px-3.5 py-2 text-xs disabled:opacity-45"
        >
          {t(upstream.enabled ? 'admin.upstream.pause' : 'admin.upstream.resume')}
        </button>
        {confirming ? (
          <>
            <button
              type="button"
              onClick={purge}
              disabled={action.pending}
              className="bg-destructive text-background px-3.5 py-2 text-xs disabled:opacity-45"
            >
              {t('admin.upstream.purgeConfirm')}
            </button>
            <button
              type="button"
              onClick={() => setConfirming(false)}
              className="text-muted-foreground px-2 py-2 text-xs"
            >
              {t('admin.upstream.purgeCancel')}
            </button>
          </>
        ) : (
          <button
            type="button"
            onClick={() => {
              setPurged(null)
              setConfirming(true)
            }}
            className="border-line-strong text-foreground border px-3.5 py-2 text-xs"
          >
            {t('admin.upstream.purge')}
          </button>
        )}
        <button
          type="button"
          onClick={() => navigate(`/admin/upstreams/${upstream.id}/edit`)}
          className="border-line-strong text-foreground border px-3.5 py-2 text-xs"
        >
          {t('admin.upstream.edit')}
        </button>
      </div>
      {purged && (
        <span role="status" className="text-muted-foreground text-[11.5px]">
          {purged.skipped > 0
            ? t('admin.cache.purgedWithSkipped', {
                removed: purged.removed,
                skipped: purged.skipped,
              })
            : t('admin.cache.purged', { removed: purged.removed })}
        </span>
      )}
      {action.errorKey && (
        <span role="alert" className="text-destructive text-[11.5px]">
          {t(action.errorKey)}
        </span>
      )}
    </div>
  )
}
