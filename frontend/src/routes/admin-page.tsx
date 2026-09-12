import { useTranslation } from 'react-i18next'

/**
 * 后台那一面的占位。
 *
 * 这一轮只立起两个面的外壳，后台的上游、访问规则、缓存、设置各自由后面的任务
 * 接手；这里先占住路由，免得它们各自再发明一套壳。
 */
export function AdminPage() {
  const { t } = useTranslation()
  return (
    <div className="bg-background flex min-h-screen flex-col items-center justify-center gap-2">
      <h1 className="text-foreground text-xl font-semibold">{t('admin.title')}</h1>
      <p className="text-muted-foreground text-sm">{t('admin.pending')}</p>
    </div>
  )
}
