import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'

import type { VersionInfo } from '@/lib/api'

/**
 * 顶栏：字标 + 版本徽标 + 通往后台的入口。
 *
 * 版本号取不到时徽标整块不出现，不换成一句「版本获取失败」——那是解释性文案，
 * 而这个数字对使用者本来就是可有可无的。
 */
export function SiteHeader({ version }: { version: VersionInfo | null }) {
  const { t } = useTranslation()
  return (
    <header className="border-border flex h-16 shrink-0 items-center justify-between border-b px-12">
      <div className="flex items-center gap-2.5">
        <span className="text-foreground font-mono text-[19px] font-semibold tracking-[-0.02em]">
          {t('app.name')}
        </span>
        {version && (
          <span className="bg-signal-soft text-primary px-1.5 py-0.5 font-mono text-[11px]">
            {t('app.version', { version: version.version })}
          </span>
        )}
      </div>
      <nav className="flex items-center gap-7">
        <Link to="/admin" className="text-muted-foreground hover:text-foreground text-[13px]">
          {t('nav.admin')}
        </Link>
      </nav>
    </header>
  )
}
