import { useState, type FormEvent, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { Input } from '@/components/ui/input'
import { useAdminAction } from '@/hooks/use-admin-action'
import {
  saveUpstream,
  toUpstreamDraft,
  type AdminUpstreamItem,
  type DefaultPolicy,
  type UpstreamDraft,
  type UpstreamKind,
} from '@/lib/api'
import { cn } from '@/lib/utils'

const KINDS: UpstreamKind[] = ['registry', 'static']
const POLICIES: DefaultPolicy[] = ['allow_all', 'deny_unless_matched']

/** 新登记一条上游时的出厂值，和后端 SaveUpstreamRequest 的默认行为对齐。 */
const BLANK: UpstreamDraft = {
  id: 0,
  host: '',
  kind: 'registry',
  origin: '',
  enabled: true,
  immutable_patterns: [],
  mutable_ttl_seconds: 300,
  default_policy: 'allow_all',
  library_completion: false,
  note: '',
}

/**
 * 添加或编辑一条上游。
 *
 * 新增和编辑共用一张表单：两者的字段集合完全相同，拆开只会有两份等价的校验，
 * 后端的两个端点（POST 新登记、PUT /:id 整条替换）认的也是同一批字段。保存是
 * 整条覆盖，所以编辑时表单要先装满这条上游此刻的登记信息，空着的字段会把库里的
 * 值一起清掉。
 */
export function UpstreamFormScreen({
  adminKey,
  upstreams,
  onUnauthorized,
  onSaved,
}: {
  adminKey: string
  upstreams: AdminUpstreamItem[]
  onUnauthorized: () => void
  onSaved: () => void
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { id } = useParams()
  const editing = upstreams.find((item) => item.id === Number(id))
  const [draft, setDraft] = useState<UpstreamDraft | null>(null)
  const action = useAdminAction(onUnauthorized)

  // 编辑时以库里那条为底：表单是这条上游此刻的样子，不是一张空表。
  const current: UpstreamDraft = draft ?? (editing ? toUpstreamDraft(editing) : { ...BLANK })
  if (id && !editing && upstreams.length > 0) {
    return (
      <div className="px-8 py-7">
        <p className="text-muted-foreground text-[13px]">{t('admin.upstream.missing')}</p>
      </div>
    )
  }

  function change(patch: Partial<UpstreamDraft>) {
    setDraft({ ...current, ...patch })
  }

  function submit(event: FormEvent) {
    event.preventDefault()
    if (current.host.trim() === '' || current.origin.trim() === '' || action.pending) {
      return
    }
    void action.run(
      () =>
        saveUpstream(adminKey, {
          ...current,
          host: current.host.trim(),
          origin: current.origin.trim(),
        }),
      (saved) => {
        onSaved()
        navigate(`/admin/upstreams/${saved.id}`)
      }
    )
  }

  return (
    <>
      <ScreenHeader title={t(editing ? 'admin.upstream.editTitle' : 'admin.upstream.addTitle')} />
      <form onSubmit={submit} className="flex max-w-[720px] flex-col px-8 pb-10">
        <Field label={t('admin.upstream.form.host')} htmlFor="upstream-host">
          <Input
            id="upstream-host"
            value={current.host}
            spellCheck={false}
            autoComplete="off"
            onChange={(event) => change({ host: event.target.value })}
            className="w-[280px] rounded-none font-mono text-[13px] md:text-[13px]"
          />
        </Field>
        <Field label={t('admin.upstream.form.origin')} htmlFor="upstream-origin">
          <Input
            id="upstream-origin"
            value={current.origin}
            spellCheck={false}
            autoComplete="off"
            onChange={(event) => change({ origin: event.target.value })}
            className="w-[360px] rounded-none font-mono text-[13px] md:text-[13px]"
          />
        </Field>
        <Field label={t('admin.upstream.form.kind')}>
          <Segmented
            label={t('admin.upstream.form.kind')}
            options={KINDS.map((kind) => ({ value: kind, label: t(`kind.${kind}`) }))}
            value={current.kind}
            onChange={(value) => change({ kind: value as UpstreamKind })}
          />
        </Field>
        <Field label={t('admin.upstream.form.defaultPolicy')}>
          <Segmented
            label={t('admin.upstream.form.defaultPolicy')}
            options={POLICIES.map((policy) => ({
              value: policy,
              label: t(`admin.rules.policy.${policy}`),
            }))}
            value={current.default_policy}
            onChange={(value) => change({ default_policy: value as DefaultPolicy })}
          />
        </Field>
        <Field label={t('admin.upstream.form.note')} htmlFor="upstream-note">
          <Input
            id="upstream-note"
            value={current.note}
            onChange={(event) => change({ note: event.target.value })}
            className="w-[360px] rounded-none text-[13px] md:text-[13px]"
          />
        </Field>

        <div className="mt-6 flex items-center gap-3">
          <button
            type="submit"
            disabled={action.pending}
            className="bg-foreground text-background px-4 py-2 text-[12.5px] disabled:opacity-45"
          >
            {t('admin.upstream.form.save')}
          </button>
          <button
            type="button"
            onClick={() => navigate(-1)}
            className="text-muted-foreground px-2 py-2 text-[12.5px]"
          >
            {t('admin.upstream.form.cancel')}
          </button>
          {action.errorKey && (
            <span role="alert" className="text-destructive text-[12.5px]">
              {t(action.errorKey)}
            </span>
          )}
        </div>
      </form>
    </>
  )
}

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor?: string
  children: ReactNode
}) {
  return (
    <div className="border-border flex flex-wrap items-center gap-6 border-b py-3.5">
      <div className="w-[220px] shrink-0">
        {htmlFor ? (
          <label htmlFor={htmlFor} className="text-foreground text-[13px]">
            {label}
          </label>
        ) : (
          <span className="text-foreground text-[13px]">{label}</span>
        )}
      </div>
      {children}
    </div>
  )
}

function Segmented({
  label,
  options,
  value,
  onChange,
}: {
  label: string
  options: { value: string; label: string }[]
  value: string
  onChange: (value: string) => void
}) {
  return (
    <div role="radiogroup" aria-label={label} className="border-line-strong flex border">
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={value === option.value}
          onClick={() => onChange(option.value)}
          className={cn(
            'px-3.5 py-[7px] text-xs',
            value === option.value
              ? 'bg-foreground text-background'
              : 'text-muted-foreground hover:text-foreground'
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}
