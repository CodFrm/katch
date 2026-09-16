import { useCallback, useState } from 'react'
import { Route, Routes } from 'react-router-dom'
import { useTranslation } from 'react-i18next'

import { AdminShell, ScreenHeader } from '@/components/admin/admin-shell'
import { LoginScreen } from '@/components/admin/login-screen'
import { useAdminUpstreamData } from '@/hooks/use-admin-data'
import { useAdminSession } from '@/hooks/use-admin-session'
import type { StatRange } from '@/lib/api'
import { CacheScreen } from '@/routes/admin/cache-screen'
import { GitMirrorsScreen } from '@/routes/admin/git-mirrors-screen'
import { ImagesScreen } from '@/routes/admin/images-screen'
import { OverviewScreen } from '@/routes/admin/overview-screen'
import { RulesScreen } from '@/routes/admin/rules-screen'
import { SettingsScreen } from '@/routes/admin/settings-screen'
import { UpstreamFormScreen } from '@/routes/admin/upstream-form-screen'
import { UpstreamScreen } from '@/routes/admin/upstream-screen'

/** 上游详情的两个页签各占一条路由：外壳一样，内容按页签换。 */
const UPSTREAM_TABS = ['overview', 'rules'] as const

/**
 * 后台那一面：没有密钥就只有登录页。
 *
 * 密钥握在浏览器里（见 useAdminSession），所以「登录」就是「手上有一把能用的
 * 密钥」。用着的密钥被轮换掉时退回登录页并说明原因，而不是把一屏空数据留在那。
 */
export function AdminPage() {
  const session = useAdminSession()

  if (!session.key) {
    return (
      <LoginScreen
        failure={session.failure}
        verifying={session.verifying}
        onSubmit={(key) => void session.signIn(key)}
      />
    )
  }
  return (
    <AdminWorkspace
      adminKey={session.key}
      onReject={session.reject}
      onSignOut={session.signOut}
      onKeyRotated={session.adopt}
    />
  )
}

/**
 * 登录之后的工作区。
 *
 * 上游登记信息与按上游的统计在这一层取一次，几个屏幕共用；区间也放在这一层，
 * 从概览点进某个上游再退回来时不会把选的区间丢掉。
 */
function AdminWorkspace({
  adminKey,
  onReject,
  onSignOut,
  onKeyRotated,
}: {
  adminKey: string
  onReject: (reason: 'unauthorized') => void
  onSignOut: () => void
  onKeyRotated: (key: string) => void
}) {
  const { t } = useTranslation()
  const [range, setRange] = useState<StatRange>('24h')
  const onUnauthorized = useCallback(() => onReject('unauthorized'), [onReject])
  const { upstreams, stats, reload } = useAdminUpstreamData(adminKey, range, onUnauthorized)

  return (
    <AdminShell stats={stats} onSignOut={onSignOut}>
      <Routes>
        <Route
          index
          element={
            <OverviewScreen
              adminKey={adminKey}
              range={range}
              onRangeChange={setRange}
              stats={stats}
              onUnauthorized={onUnauthorized}
            />
          }
        />
        <Route
          path="upstreams/new"
          element={
            <UpstreamFormScreen
              adminKey={adminKey}
              upstreams={upstreams}
              onUnauthorized={onUnauthorized}
              onSaved={reload}
            />
          }
        />
        <Route
          path="upstreams/:id/edit"
          element={
            <UpstreamFormScreen
              adminKey={adminKey}
              upstreams={upstreams}
              onUnauthorized={onUnauthorized}
              onSaved={reload}
            />
          }
        />
        {UPSTREAM_TABS.map((tab) => (
          <Route
            key={tab}
            path={tab === 'overview' ? 'upstreams/:id' : 'upstreams/:id/rules'}
            element={
              <UpstreamScreen
                adminKey={adminKey}
                range={range}
                stats={stats}
                upstreams={upstreams}
                tab={tab}
                onUnauthorized={onUnauthorized}
                onChange={reload}
              />
            }
          />
        ))}
        <Route
          path="cache"
          element={
            <CacheScreen
              adminKey={adminKey}
              upstreams={upstreams}
              onUnauthorized={onUnauthorized}
            />
          }
        />
        <Route
          path="git"
          element={<GitMirrorsScreen adminKey={adminKey} onUnauthorized={onUnauthorized} />}
        />
        <Route
          path="images"
          element={
            <ImagesScreen
              adminKey={adminKey}
              upstreams={upstreams}
              onUnauthorized={onUnauthorized}
            />
          }
        />
        {/* 全局规则没有自己的上游，从设置页进来：它先于每个上游自己的规则求值。 */}
        <Route
          path="rules"
          element={
            <>
              <ScreenHeader
                title={t('admin.rules.globalTitle')}
                subtitle={
                  <span className="text-muted-foreground">{t('admin.rules.globalSubtitle')}</span>
                }
              />
              <RulesScreen
                adminKey={adminKey}
                upstream={null}
                onUnauthorized={onUnauthorized}
                onUpstreamChange={reload}
              />
            </>
          }
        />
        <Route
          path="settings"
          element={
            <SettingsScreen
              adminKey={adminKey}
              onUnauthorized={onUnauthorized}
              onKeyRotated={onKeyRotated}
            />
          }
        />
      </Routes>
    </AdminShell>
  )
}
