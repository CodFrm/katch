import { useState, type FormEvent, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { RuleTester } from '@/components/admin/rule-tester'
import { Input } from '@/components/ui/input'
import { useAdminRules } from '@/hooks/use-admin-data'
import { useAdminAction } from '@/hooks/use-admin-action'
import {
  deleteRule,
  saveRule,
  saveUpstream,
  toUpstreamDraft,
  type AdminRuleItem,
  type AdminUpstreamItem,
  type DefaultPolicy,
  type RuleAction,
} from '@/lib/api'
import { cn } from '@/lib/utils'

const POLICIES: DefaultPolicy[] = ['allow_all', 'deny_unless_matched']
const ACTIONS: RuleAction[] = ['allow', 'deny']

/** 正在编辑的那条规则，id 为 0 即新增。 */
interface RuleDraft {
  id: number
  action: RuleAction
  pattern: string
  note: string
}

const EMPTY_DRAFT: RuleDraft = { id: 0, action: 'allow', pattern: '', note: '' }

/**
 * 访问规则：左边是默认策略与规则表，右边是规则测试器。
 *
 * 同一块界面服务两个范围：某个上游自己的规则（upstream 给出时），以及先于所有
 * 上游求值的全局规则（upstream 为 null）。两者的字段、操作和测试器完全一样，
 * 拆成两个屏幕只会有两份等价的实现。
 *
 * 表里不给行号，也不在前端按具体度重排：那个序是拉取路径上那套定序算出来的，
 * 在这里复算一遍，一旦和后端有出入，界面就会理直气壮地指错规则。「谁先说了算」
 * 由测试器按一次真实求值回答。
 */
export function RulesScreen({
  adminKey,
  upstream,
  onUnauthorized,
  onUpstreamChange,
}: {
  adminKey: string
  /** 这一面属于哪个上游；全局规则那一面是 null。 */
  upstream: AdminUpstreamItem | null
  onUnauthorized: () => void
  /** 默认策略写在上游记录上，改完要让外面那份上游数据跟上。 */
  onUpstreamChange: () => void
}) {
  const { t } = useTranslation()
  const loaded = useAdminRules(adminKey, onUnauthorized)
  const all = loaded.data
  const scopeID = upstream?.id ?? 0
  const scoped = all.filter((rule) => rule.upstream_id === scopeID)
  const globalCount = all.filter((rule) => rule.upstream_id === 0).length
  const [draft, setDraft] = useState<RuleDraft | null>(null)
  const action = useAdminAction(onUnauthorized)
  const policy: DefaultPolicy = upstream ? toUpstreamDraft(upstream).default_policy : 'allow_all'

  function changePolicy(next: DefaultPolicy) {
    if (!upstream || next === policy) {
      return
    }
    // 整条写回去：保存是覆盖而不是打补丁，只送一个字段会把别的登记信息清零。
    void action.run(
      () => saveUpstream(adminKey, { ...toUpstreamDraft(upstream), default_policy: next }),
      onUpstreamChange
    )
  }

  function submitDraft(event: FormEvent) {
    event.preventDefault()
    if (!draft || draft.pattern.trim() === '' || action.pending) {
      return
    }
    void action.run(
      () =>
        saveRule(adminKey, {
          id: draft.id,
          upstream_id: scopeID,
          action: draft.action,
          pattern: draft.pattern.trim(),
          note: draft.note,
        }),
      () => {
        setDraft(null)
        loaded.reload()
      }
    )
  }

  function remove(rule: AdminRuleItem) {
    void action.run(
      () => deleteRule(adminKey, rule.id),
      () => {
        loaded.reload()
        onUpstreamChange()
      }
    )
  }

  return (
    <div className="flex min-h-0 flex-1">
      <div className="flex min-w-0 flex-1 flex-col gap-6 px-8 py-6">
        <section className="border-border bg-muted/40 flex flex-wrap items-center gap-4 border px-[18px] py-3.5">
          <span className="text-foreground text-[13px]">{t('admin.rules.defaultPolicy')}</span>
          <div
            role="radiogroup"
            aria-label={t('admin.rules.defaultPolicy')}
            className="border-line-strong bg-background flex border"
          >
            {POLICIES.map((option) => (
              <button
                key={option}
                type="button"
                role="radio"
                aria-checked={policy === option}
                disabled={!upstream}
                onClick={() => changePolicy(option)}
                className={cn(
                  'px-3.5 py-[7px] text-xs disabled:opacity-45',
                  policy === option
                    ? 'bg-foreground text-background'
                    : 'text-muted-foreground hover:text-foreground'
                )}
              >
                {t(`admin.rules.policy.${option}`)}
              </button>
            ))}
          </div>
          <span className="text-muted-foreground text-xs">
            {upstream ? t(`admin.rules.policyHint.${policy}`) : t('admin.rules.globalScopeHint')}
          </span>
          {upstream && globalCount > 0 && (
            <span className="text-muted-foreground text-xs">
              {t('admin.rules.globalCount', { total: globalCount })}
            </span>
          )}
          <span className="flex-1" />
          <button
            type="button"
            onClick={() => setDraft({ ...EMPTY_DRAFT })}
            className="text-primary text-[12.5px]"
          >
            {t('admin.rules.add')}
          </button>
        </section>

        {action.errorKey && (
          <p role="alert" className="text-destructive text-[12.5px]">
            {t(action.errorKey)}
          </p>
        )}

        {draft && (
          <form
            onSubmit={submitDraft}
            className="border-border flex flex-wrap items-end gap-4 border px-[18px] py-4"
          >
            <div className="flex flex-col gap-1.5">
              <span className="text-ink-3 text-[11px] tracking-[0.12em]">
                {t('admin.rules.columns.action')}
              </span>
              <div
                role="radiogroup"
                aria-label={t('admin.rules.columns.action')}
                className="border-line-strong flex border"
              >
                {ACTIONS.map((option) => (
                  <button
                    key={option}
                    type="button"
                    role="radio"
                    aria-checked={draft.action === option}
                    onClick={() => setDraft({ ...draft, action: option })}
                    className={cn(
                      'px-3 py-1.5 text-xs',
                      draft.action === option
                        ? 'bg-foreground text-background'
                        : 'text-muted-foreground'
                    )}
                  >
                    {t(`admin.rules.action.${option}`)}
                  </button>
                ))}
              </div>
            </div>
            <Field label={t('admin.rules.columns.pattern')} htmlFor="rule-pattern">
              <Input
                id="rule-pattern"
                value={draft.pattern}
                spellCheck={false}
                autoComplete="off"
                onChange={(event) => setDraft({ ...draft, pattern: event.target.value })}
                className="w-[260px] rounded-none font-mono text-[13px] md:text-[13px]"
              />
            </Field>
            <Field label={t('admin.rules.columns.note')} htmlFor="rule-note">
              <Input
                id="rule-note"
                value={draft.note}
                onChange={(event) => setDraft({ ...draft, note: event.target.value })}
                className="w-[260px] rounded-none text-[13px] md:text-[13px]"
              />
            </Field>
            <div className="flex items-center gap-2">
              <button
                type="submit"
                disabled={action.pending}
                className="bg-foreground text-background px-3.5 py-2 text-xs disabled:opacity-45"
              >
                {t('admin.rules.save')}
              </button>
              <button
                type="button"
                onClick={() => setDraft(null)}
                className="text-muted-foreground px-2 py-2 text-xs"
              >
                {t('admin.rules.cancel')}
              </button>
            </div>
          </form>
        )}

        <table aria-label={t('admin.rules.title')} className="w-full text-left">
          <thead>
            <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
              <th scope="col" className="w-[92px] py-2 font-normal">
                {t('admin.rules.columns.action')}
              </th>
              <th scope="col" className="py-2 font-normal">
                {t('admin.rules.columns.pattern')}
              </th>
              <th scope="col" className="py-2 font-normal">
                {t('admin.rules.columns.note')}
              </th>
              <th scope="col" className="w-[120px] py-2 font-normal">
                {t('admin.rules.columns.operations')}
              </th>
            </tr>
          </thead>
          <tbody>
            {scoped.map((rule) => (
              <tr key={rule.id} className="border-border border-b">
                <td className="py-2.5">
                  <span
                    className={cn(
                      'text-background px-2 py-[3px] text-[11px] tracking-[0.04em]',
                      rule.action === 'deny' ? 'bg-destructive' : 'bg-ok'
                    )}
                  >
                    {t(`admin.rules.action.${rule.action}`)}
                  </span>
                </td>
                <td className="text-foreground py-2.5 font-mono text-[13px]">{rule.pattern}</td>
                <td className="text-muted-foreground py-2.5 text-[13px]">{rule.note}</td>
                <td className="py-2.5">
                  <div className="flex items-center gap-3">
                    <button
                      type="button"
                      onClick={() =>
                        setDraft({
                          id: rule.id,
                          action: rule.action,
                          pattern: rule.pattern,
                          note: rule.note,
                        })
                      }
                      className="text-muted-foreground hover:text-foreground text-[12.5px]"
                    >
                      {t('admin.rules.edit')}
                    </button>
                    <button
                      type="button"
                      onClick={() => remove(rule)}
                      className="text-muted-foreground hover:text-destructive text-[12.5px]"
                    >
                      {t('admin.rules.remove')}
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {scoped.length === 0 && (
          <p className="text-muted-foreground text-[13px]">{t('admin.rules.empty')}</p>
        )}
      </div>

      <RuleTester adminKey={adminKey} host={upstream?.host ?? ''} onUnauthorized={onUnauthorized} />
    </div>
  )
}

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-ink-3 text-[11px] tracking-[0.12em]">
        {label}
      </label>
      {children}
    </div>
  )
}
