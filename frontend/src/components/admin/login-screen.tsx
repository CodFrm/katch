import { ShieldAlert } from 'lucide-react'
import { useEffect, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'
import { fetchVersion, type AdminFailure, type VersionInfo } from '@/lib/api'

/**
 * 登录：一把密钥，两行说明。
 *
 * 失败只说得出两件事——这把密钥进不去，或者够不到这台 katch。后端对「密钥错了」
 * 和「没填密钥」返回完全一样的 401（它刻意不让人分辨），所以界面也不装作能分。
 * 后端那句英文 msg 一个字都不往外贴。
 */
export function LoginScreen({
  failure,
  verifying,
  onSubmit,
}: {
  failure: AdminFailure | null
  verifying: boolean
  onSubmit: (key: string) => void
}) {
  const { t } = useTranslation()
  const [key, setKey] = useState('')
  const [visible, setVisible] = useState(false)
  const [version, setVersion] = useState<VersionInfo | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    void fetchVersion(controller.signal).then((info) => {
      if (!controller.signal.aborted) {
        setVersion(info)
      }
    })
    return () => controller.abort()
  }, [])

  function submit(event: FormEvent) {
    event.preventDefault()
    if (!verifying) {
      onSubmit(key)
    }
  }

  return (
    <div className="bg-background flex min-h-screen">
      <aside className="bg-foreground text-background hidden w-[596px] shrink-0 flex-col justify-between p-14 lg:flex">
        <div className="flex flex-col gap-5">
          <span className="font-mono text-[40px] leading-none font-semibold tracking-[-0.04em]">
            {t('app.name')}
          </span>
          <p className="text-sm leading-[1.8] opacity-55">{t('login.pitch')}</p>
        </div>
        {version && (
          <dl className="flex items-center justify-between border-background/15 border-t py-[11px]">
            <dt className="text-[11.5px] opacity-45">{t('login.versionLabel')}</dt>
            <dd className="font-mono text-[11.5px] opacity-75">
              {t('app.version', { version: version.version })}
            </dd>
          </dl>
        )}
      </aside>

      <main className="flex flex-1 items-center justify-center px-20">
        <form onSubmit={submit} className="flex w-[380px] flex-col">
          <h1 className="text-foreground text-[26px] leading-none font-semibold tracking-[-0.02em]">
            {t('login.title')}
          </h1>
          <p className="text-muted-foreground mt-3 text-[13px] leading-[1.7]">
            {t('login.subtitle')}
          </p>

          <div className="mt-7 flex items-center justify-between pb-2">
            <label htmlFor="admin-key" className="text-ink-3 text-[11px] tracking-[0.12em]">
              {t('login.keyLabel')}
            </label>
            <button
              type="button"
              onClick={() => setVisible((shown) => !shown)}
              className="text-muted-foreground hover:text-foreground text-[11.5px]"
            >
              {visible ? t('login.hide') : t('login.show')}
            </button>
          </div>
          <Input
            id="admin-key"
            type={visible ? 'text' : 'password'}
            value={key}
            autoComplete="off"
            spellCheck={false}
            onChange={(event) => setKey(event.target.value)}
            className="border-foreground text-foreground h-auto rounded-none border-[1.5px] bg-transparent px-3.5 py-3 font-mono text-sm shadow-none focus-visible:border-foreground focus-visible:ring-0 md:text-sm dark:bg-transparent"
          />
          {failure && (
            <p role="alert" className="text-destructive mt-2.5 text-[12.5px] leading-[1.6]">
              {failure === 'unauthorized' ? t('login.invalid') : t('login.unreachable')}
            </p>
          )}

          <button
            type="submit"
            disabled={verifying || key.length === 0}
            className="bg-foreground text-background mt-[18px] py-3 text-[13.5px] disabled:opacity-45"
          >
            {t('login.submit')}
          </button>

          <p className="text-ink-3 border-border mt-[26px] flex gap-2.5 border-t pt-4 text-[11.5px] leading-[1.7]">
            <ShieldAlert className="mt-[3px] size-3.5 shrink-0" aria-hidden />
            <span>{t('login.loopbackNotice')}</span>
          </p>
        </form>
      </main>
    </div>
  )
}
