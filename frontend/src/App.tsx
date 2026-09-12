import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

interface VersionInfo {
  version: string
  commit: string
}

export default function App() {
  const { t } = useTranslation()
  const [info, setInfo] = useState<VersionInfo | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    fetch('/api/v1/system/version')
      .then((r) => {
        if (!r.ok) throw new Error(String(r.status))
        return r.json()
      })
      .then((d: VersionInfo) => setInfo(d))
      .catch(() => setFailed(true))
  }, [])

  return (
    <main className="mx-auto flex min-h-screen max-w-2xl flex-col justify-center gap-2 px-6">
      <h1 className="text-3xl font-semibold tracking-tight">{t('app.name')}</h1>
      <p className="text-muted-foreground">{t('app.tagline')}</p>
      <p className="text-muted-foreground text-sm">
        {failed
          ? t('system.error')
          : info
            ? `${t('system.version')} ${info.version} (${info.commit})`
            : t('system.loading')}
      </p>
    </main>
  )
}
