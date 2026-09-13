import { FileCog } from 'lucide-react'
import { useState, type FormEvent, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { Input } from '@/components/ui/input'
import { useAdminAction } from '@/hooks/use-admin-action'
import { useAdminRules, useAdminSettings } from '@/hooks/use-admin-data'
import { rotateAdminKey, saveSettings } from '@/lib/api'
import { formatBytes, formatDuration, parseBytes, parseDuration } from '@/lib/format'
import { readBoolSetting, readIntSetting, readStringSetting } from '@/lib/settings'
import { cn } from '@/lib/utils'

/**
 * 那些不在这里改的配置住在哪。
 *
 * 是个机器路径，不是文案：翻译它只会让人照着译文去找一个不存在的文件。
 */
const CONFIG_PATH = 'configs/config.yaml'

/** 密钥的长度下限，和后端 RotateAdminKeyRequest 的 min=16 对齐。 */
const MIN_KEY_LENGTH = 16

/**
 * 设置：只放运行时项。
 *
 * 顶上那条提示不是装饰——两类配置的边界必须画在界面上，而不是留给用户猜：监听
 * 地址、数据库、日志在 configs/config.yaml 里（进程起不来就改不了的东西，改完
 * 要重启），这一页里的每一项改完立刻生效，不必重启进程。
 */
export function SettingsScreen({
  adminKey,
  onUnauthorized,
  onKeyRotated,
}: {
  adminKey: string
  onUnauthorized: () => void
  /** 轮换成功：浏览器手上那把当场作废，要立刻换上新的，否则下一个请求就 401。 */
  onKeyRotated: (key: string) => void
}) {
  const { t } = useTranslation()
  const settings = useAdminSettings(adminKey, onUnauthorized)
  const rules = useAdminRules(adminKey, onUnauthorized)
  const action = useAdminAction(onUnauthorized)
  // 只记改动过的键：保存时送的就是这些，没碰过的项不该被一次「保存更改」重写。
  const [edits, setEdits] = useState<Record<string, unknown>>({})
  const [localError, setLocalError] = useState<string | null>(null)
  const [savedAt, setSavedAt] = useState<string | null>(null)
  const list = settings.data

  function edit(key: string, value: unknown) {
    setSavedAt(null)
    setLocalError(null)
    action.clearError()
    setEdits((current) => ({ ...current, [key]: value }))
  }

  function textValue(key: string, shown: string): string {
    const pending = edits[key]
    return pending === undefined ? shown : String(pending)
  }

  /** 把一个写成人话的时长/容量解回数，解不出来时留一个记号，保存时拦住。 */
  function editParsed(key: string, raw: string, parse: (text: string) => number | null) {
    const parsed = parse(raw)
    setSavedAt(null)
    action.clearError()
    setLocalError(parsed === null ? 'admin.settings.unreadableValue' : null)
    setEdits((current) => ({ ...current, [key]: parsed === null ? raw : parsed }))
  }

  function save() {
    const payload: Record<string, unknown> = {}
    for (const [key, value] of Object.entries(edits)) {
      if (typeof value === 'string' && NUMERIC_KEYS.has(key)) {
        // 解不出来的时长/容量停在这里：把 `3 PB?` 当成 0 送出去，后端会老老实实
        // 存下一个谁也没要求过的值。
        setLocalError('admin.settings.unreadableValue')
        return
      }
      payload[key] = value
    }
    if (Object.keys(payload).length === 0) {
      return
    }
    void action.run(
      () => saveSettings(adminKey, payload),
      () => {
        setEdits({})
        setSavedAt(nowTime())
        settings.reload()
      }
    )
  }

  // 开关的当前值：改过就用改过的，没改过就用库里那个。
  const publicHome =
    edits.public_homepage === undefined
      ? readBoolSetting(list, 'public_homepage')
      : edits.public_homepage === true
  const quota = readIntSetting(list, 'cache_quota_bytes')
  const ttl = readIntSetting(list, 'mutable_ttl_seconds')
  const timeout = readIntSetting(list, 'origin_timeout_seconds')

  return (
    <>
      <ScreenHeader
        title={t('admin.settings.title')}
        subtitle={<span className="text-muted-foreground">{t('admin.settings.subtitle')}</span>}
        actions={
          <div className="flex items-center gap-4">
            {savedAt && (
              <span role="status" className="text-ink-3 text-[11.5px]">
                {t('admin.settings.saved', { time: savedAt })}
              </span>
            )}
            {(action.errorKey || localError) && (
              <span role="alert" className="text-destructive text-[12px]">
                {t(action.errorKey ?? localError ?? '')}
              </span>
            )}
            <button
              type="button"
              onClick={save}
              disabled={action.pending || Object.keys(edits).length === 0}
              className="bg-foreground text-background px-3.5 py-2 text-[12.5px] disabled:opacity-45"
            >
              {t('admin.settings.save')}
            </button>
          </div>
        }
      />

      <div className="border-border bg-muted/40 flex items-center gap-2.5 border-b px-8 py-3.5">
        <FileCog className="text-ink-3 size-3.5 shrink-0" aria-hidden />
        <span className="text-muted-foreground text-[13px]">
          {t('admin.settings.configNoticeLead')}
        </span>
        <span className="text-foreground font-mono text-xs">{CONFIG_PATH}</span>
        <span className="text-muted-foreground text-[13px]">
          {t('admin.settings.configNoticeTail')}
        </span>
      </div>

      <div className="flex flex-col px-8 pb-10">
        <Group label={t('admin.settings.groups.site')} />
        <Row
          label={t('admin.settings.fields.siteDomain')}
          htmlFor="setting-site-domain"
          hint={t('admin.settings.hints.siteDomain')}
        >
          <Input
            id="setting-site-domain"
            value={textValue('site_domain', readStringSetting(list, 'site_domain'))}
            onChange={(event) => edit('site_domain', event.target.value)}
            className="w-[240px] rounded-none font-mono text-[13px] md:text-[13px]"
          />
        </Row>
        <Row label={t('admin.settings.fields.siteName')} htmlFor="setting-site-name">
          <Input
            id="setting-site-name"
            value={textValue('site_name', readStringSetting(list, 'site_name'))}
            onChange={(event) => edit('site_name', event.target.value)}
            className="w-[280px] rounded-none text-[13px] md:text-[13px]"
          />
        </Row>
        <Row
          label={t('admin.settings.fields.publicHomepage')}
          hint={t('admin.settings.hints.publicHomepage')}
        >
          <Switch
            label={t('admin.settings.fields.publicHomepage')}
            checked={publicHome}
            onChange={(next) => edit('public_homepage', next)}
            caption={t(
              publicHome ? 'admin.settings.states.publicOn' : 'admin.settings.states.publicOff'
            )}
          />
        </Row>

        <Group label={t('admin.settings.groups.cache')} />
        <Row
          label={t('admin.settings.fields.quota')}
          htmlFor="setting-quota"
          hint={t('admin.settings.hints.quota')}
        >
          <div className="flex items-center gap-3">
            <Input
              id="setting-quota"
              value={textValue(
                'cache_quota_bytes',
                `${formatBytes(quota).value} ${formatBytes(quota).unit}`
              )}
              onChange={(event) => editParsed('cache_quota_bytes', event.target.value, parseBytes)}
              className="w-[120px] rounded-none font-mono text-[13px] md:text-[13px]"
            />
            <span className="text-muted-foreground text-[13px]">
              {t('admin.settings.fields.watermarkShort')}
            </span>
            <Input
              aria-label={t('admin.settings.fields.watermark')}
              inputMode="numeric"
              value={textValue(
                'cache_reclaim_percent',
                String(readIntSetting(list, 'cache_reclaim_percent'))
              )}
              onChange={(event) => edit('cache_reclaim_percent', Number(event.target.value))}
              className="w-[90px] rounded-none font-mono text-[13px] md:text-[13px]"
            />
          </div>
        </Row>
        <Row
          label={t('admin.settings.fields.mutableTTL')}
          htmlFor="setting-ttl"
          hint={t('admin.settings.hints.mutableTTL')}
        >
          <Input
            id="setting-ttl"
            value={textValue('mutable_ttl_seconds', formatDuration(ttl))}
            onChange={(event) =>
              editParsed('mutable_ttl_seconds', event.target.value, parseDuration)
            }
            className="w-[120px] rounded-none font-mono text-[13px] md:text-[13px]"
          />
        </Row>
        <Row
          label={t('admin.settings.fields.immutable')}
          hint={t('admin.settings.hints.immutable')}
        >
          <span className="text-muted-foreground text-[13px]">
            {t('admin.settings.states.immutable')}
          </span>
        </Row>

        <Group label={t('admin.settings.groups.origin')} />
        <Row
          label={t('admin.settings.fields.concurrency')}
          htmlFor="setting-concurrency"
          hint={t('admin.settings.hints.concurrency')}
        >
          <Input
            id="setting-concurrency"
            inputMode="numeric"
            value={textValue(
              'origin_concurrency',
              String(readIntSetting(list, 'origin_concurrency'))
            )}
            onChange={(event) => edit('origin_concurrency', Number(event.target.value))}
            className="w-[120px] rounded-none font-mono text-[13px] md:text-[13px]"
          />
        </Row>
        <Row label={t('admin.settings.fields.timeout')} htmlFor="setting-timeout">
          <div className="flex items-center gap-3">
            <Input
              id="setting-timeout"
              value={textValue('origin_timeout_seconds', formatDuration(timeout))}
              onChange={(event) =>
                editParsed('origin_timeout_seconds', event.target.value, parseDuration)
              }
              className="w-[120px] rounded-none font-mono text-[13px] md:text-[13px]"
            />
            <span className="text-muted-foreground text-[13px]">
              {t('admin.settings.fields.retriesShort')}
            </span>
            <Input
              aria-label={t('admin.settings.fields.retries')}
              inputMode="numeric"
              value={textValue('origin_retries', String(readIntSetting(list, 'origin_retries')))}
              onChange={(event) => edit('origin_retries', Number(event.target.value))}
              className="w-[90px] rounded-none font-mono text-[13px] md:text-[13px]"
            />
          </div>
        </Row>

        <Group label={t('admin.settings.groups.access')} />
        <Row label={t('admin.settings.fields.adminKey')}>
          <RotateKey adminKey={adminKey} onUnauthorized={onUnauthorized} onRotated={onKeyRotated} />
        </Row>
        <Row
          label={t('admin.settings.fields.globalRules')}
          hint={t('admin.settings.hints.globalRules')}
        >
          <Link
            to="/admin/rules"
            className="text-muted-foreground hover:text-foreground text-[13px]"
          >
            {t('admin.settings.globalRulesLink', {
              total: rules.data.filter((rule) => rule.upstream_id === 0).length,
            })}
          </Link>
        </Row>
      </div>
    </>
  )
}

/** 送给后端时要按数走的那几项。字符串停在这里就是没解出来的输入。 */
const NUMERIC_KEYS = new Set([
  'cache_quota_bytes',
  'cache_reclaim_percent',
  'mutable_ttl_seconds',
  'origin_concurrency',
  'origin_timeout_seconds',
  'origin_retries',
])

function nowTime(): string {
  const at = new Date()
  return `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}`
}

function Group({ label }: { label: string }) {
  return (
    <div className="border-border mt-6 border-b pb-2 first:mt-0">
      <span className="text-ink-3 text-[11px] tracking-[0.12em]">{label}</span>
    </div>
  )
}

function Row({
  label,
  hint,
  htmlFor,
  children,
}: {
  label: string
  hint?: string
  htmlFor?: string
  children: ReactNode
}) {
  return (
    <div className="border-border flex flex-wrap items-center gap-6 border-b py-3.5">
      <div className="flex w-[300px] shrink-0 flex-col gap-1">
        {htmlFor ? (
          <label htmlFor={htmlFor} className="text-foreground text-[13px]">
            {label}
          </label>
        ) : (
          <span className="text-foreground text-[13px]">{label}</span>
        )}
        {hint && <span className="text-muted-foreground text-xs">{hint}</span>}
      </div>
      {children}
    </div>
  )
}

function Switch({
  label,
  checked,
  caption,
  onChange,
}: {
  label: string
  checked: boolean
  caption: string
  onChange: (next: boolean) => void
}) {
  return (
    <div className="flex items-center gap-3">
      <button
        type="button"
        role="switch"
        aria-label={label}
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={cn(
          'flex h-[19px] w-[34px] items-center p-0.5 transition-colors',
          checked ? 'bg-ok justify-end' : 'bg-line-strong justify-start'
        )}
      >
        <span className="bg-background size-[15px]" />
      </button>
      <span className="text-muted-foreground text-[13px]">{caption}</span>
    </div>
  )
}

/**
 * 轮换管理密钥。
 *
 * 换完不回显新密钥，也不要求再输一遍当前密钥（它已经在 Authorization 头上验过）。
 * 成功之后把新的那把交给会话，否则发起轮换的人会被自己的操作踢回登录页。
 */
function RotateKey({
  adminKey,
  onUnauthorized,
  onRotated,
}: {
  adminKey: string
  onUnauthorized: () => void
  onRotated: (key: string) => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [next, setNext] = useState('')
  const action = useAdminAction(onUnauthorized)

  function submit(event: FormEvent) {
    event.preventDefault()
    if (next.length < MIN_KEY_LENGTH || action.pending) {
      return
    }
    void action.run(
      () => rotateAdminKey(adminKey, next),
      () => {
        setOpen(false)
        setNext('')
        onRotated(next)
      }
    )
  }

  if (!open) {
    return (
      <div className="flex items-center gap-4">
        <span className="text-ink-3 font-mono text-[13px]">{'•'.repeat(16)}</span>
        <button type="button" onClick={() => setOpen(true)} className="text-primary text-[13px]">
          {t('admin.settings.rotate')}
        </button>
      </div>
    )
  }

  return (
    <form onSubmit={submit} className="flex flex-wrap items-center gap-3">
      <label htmlFor="new-admin-key" className="sr-only">
        {t('admin.settings.newKey')}
      </label>
      <Input
        id="new-admin-key"
        type="password"
        value={next}
        autoComplete="new-password"
        onChange={(event) => setNext(event.target.value)}
        className="w-[240px] rounded-none font-mono text-[13px] md:text-[13px]"
      />
      <span className="text-ink-3 text-[11.5px]">
        {t('admin.settings.keyLength', { min: MIN_KEY_LENGTH })}
      </span>
      <button
        type="submit"
        disabled={action.pending || next.length < MIN_KEY_LENGTH}
        className="bg-foreground text-background px-3.5 py-2 text-xs disabled:opacity-45"
      >
        {t('admin.settings.rotateConfirm')}
      </button>
      <button
        type="button"
        onClick={() => setOpen(false)}
        className="text-muted-foreground text-xs"
      >
        {t('admin.settings.cancel')}
      </button>
      {action.errorKey && (
        <span role="alert" className="text-destructive text-xs">
          {t(action.errorKey)}
        </span>
      )}
    </form>
  )
}
