import { useCallback, useState } from 'react'
import { Route, Routes } from 'react-router-dom'

import { AdminShell } from '@/components/admin/admin-shell'
import { LoginScreen } from '@/components/admin/login-screen'
import { useAdminUpstreamData } from '@/hooks/use-admin-data'
import { useAdminSession } from '@/hooks/use-admin-session'
import type { StatRange } from '@/lib/api'
import { OverviewScreen } from '@/routes/admin/overview-screen'
import { UpstreamScreen } from '@/routes/admin/upstream-screen'

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
    <AdminWorkspace adminKey={session.key} onReject={session.reject} onSignOut={session.signOut} />
  )
}

/**
 * 登录之后的工作区。
 *
 * 上游登记信息与按上游的统计在这一层取一次，两个屏幕共用；区间也放在这一层，
 * 从概览点进某个上游再退回来时不会把选的区间丢掉。
 */
function AdminWorkspace({
  adminKey,
  onReject,
  onSignOut,
}: {
  adminKey: string
  onReject: (reason: 'unauthorized') => void
  onSignOut: () => void
}) {
  const [range, setRange] = useState<StatRange>('24h')
  const onUnauthorized = useCallback(() => onReject('unauthorized'), [onReject])
  const { upstreams, stats } = useAdminUpstreamData(adminKey, range, onUnauthorized)

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
          path="upstreams/:id"
          element={
            <UpstreamScreen
              adminKey={adminKey}
              range={range}
              stats={stats}
              upstreams={upstreams}
              onUnauthorized={onUnauthorized}
            />
          }
        />
      </Routes>
    </AdminShell>
  )
}
