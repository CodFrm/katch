import { CircleCheck, CircleSlash } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'
import { useAdminAction } from '@/hooks/use-admin-action'
import { testRule, type RuleTestResult, type RuleTraceStep } from '@/lib/api'
import { cn } from '@/lib/utils'

/** 被拒绝的拉取在客户端看到的状态码。放行不写码：放行之后还可能因为别的原因失败。 */
const DENIED_STATUS = '403'

/**
 * 规则测试器：粘一个资源地址，直接给判定、命中第几条、以及完整的匹配过程。
 *
 * 它是决策 15 的配套。具体度定序换来了确定性，代价是「为什么是这条命中」不再显然：
 * 一张按前缀长短排出来的表，人是看不出谁先说了算的。所以这里三样都要给，少一样
 * 就又回到「对着一份看不出优先级的列表自己推」。
 *
 * 判定、命中的那一条和过程都来自后端的同一次求值——不在前端复算一遍具体度定序：
 * 界面上算出来的顺序一旦和拉取路径上真正跑的那套有出入，这个面板就在撒谎。
 */
export function RuleTester({
  adminKey,
  host,
  onUnauthorized,
}: {
  adminKey: string
  /** 试算发给哪个上游；全局那一面留空，后端按整条拉取路径自己解析。 */
  host: string
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const [path, setPath] = useState('')
  const [result, setResult] = useState<RuleTestResult | null>(null)
  const action = useAdminAction(onUnauthorized)

  function submit(event: FormEvent) {
    event.preventDefault()
    if (path.trim() === '' || action.pending) {
      return
    }
    void action.run(() => testRule(adminKey, host, path.trim()), setResult)
  }

  return (
    <aside className="border-border bg-muted/40 flex w-[352px] shrink-0 flex-col gap-[18px] border-l px-7 py-6">
      <div className="flex flex-col gap-2.5">
        <h2 className="text-foreground text-[13.5px] font-semibold">{t('admin.rules.tester')}</h2>
        <p className="text-muted-foreground text-xs leading-[1.7]">{t('admin.rules.testerHint')}</p>
      </div>

      <form onSubmit={submit} className="flex flex-col gap-2.5">
        <label htmlFor="rule-test-path" className="sr-only">
          {t('admin.rules.testerLabel')}
        </label>
        <Input
          id="rule-test-path"
          value={path}
          spellCheck={false}
          autoComplete="off"
          placeholder={t('admin.rules.testerPlaceholder')}
          onChange={(event) => setPath(event.target.value)}
          className="border-foreground bg-background h-auto rounded-none border-[1.5px] px-3.5 py-3 font-mono text-[13px] shadow-none focus-visible:border-foreground focus-visible:ring-0 md:text-[13px]"
        />
        <button
          type="submit"
          disabled={action.pending || path.trim() === ''}
          className="bg-foreground text-background py-2 text-[12.5px] disabled:opacity-45"
        >
          {t('admin.rules.testerSubmit')}
        </button>
      </form>

      {action.errorKey && (
        <p role="alert" className="text-destructive text-xs leading-[1.6]">
          {t(action.errorKey)}
        </p>
      )}

      {result && <Verdict result={result} />}
    </aside>
  )
}

/** 判定卡 + 匹配过程。两者一起出现：结论没有过程就只是一句断言。 */
function Verdict({ result }: { result: RuleTestResult }) {
  const { t } = useTranslation()
  // 「第几条」数的是求值过程里的第几步，不是规则表里的第几行：表没有人工顺序，
  // 求值顺序才是决定结果的那个序（后端的 ListRules 因此刻意不给行号）。
  const decisive = result.trace.findIndex((step) => step.decisive) + 1
  const matched = result.matched_rule

  return (
    <div className="flex flex-col gap-[18px]">
      <div
        role="status"
        aria-label={t('admin.rules.verdictLabel')}
        className={cn(
          'bg-background flex flex-col gap-2 border-l-[3px] px-[18px] py-4',
          result.allowed ? 'border-l-ok' : 'border-l-destructive'
        )}
      >
        <div className="flex items-center gap-2.5">
          {result.allowed ? (
            <CircleCheck className="text-ok size-[17px]" aria-hidden />
          ) : (
            <CircleSlash className="text-destructive size-[17px]" aria-hidden />
          )}
          <span
            className={cn(
              'text-[17px] font-semibold',
              result.allowed ? 'text-ok' : 'text-destructive'
            )}
          >
            {t(result.allowed ? 'admin.rules.allowed' : 'admin.rules.denied')}
          </span>
          {!result.allowed && <span className="text-ink-3 font-mono text-xs">{DENIED_STATUS}</span>}
        </div>
        <p className="text-muted-foreground text-xs leading-[1.6]">
          {matched
            ? t('admin.rules.verdictRule', { index: decisive })
            : t(`admin.rules.verdictDefault.${result.default_policy}`)}
          {matched?.note ? t('admin.rules.verdictNote', { note: matched.note }) : ''}
        </p>
      </div>

      <div className="flex flex-col gap-2">
        <h3 className="text-ink-3 text-[11px] tracking-[0.12em]">{t('admin.rules.trace')}</h3>
        <ol aria-label={t('admin.rules.trace')} className="flex flex-col">
          {result.trace.map((step, index) => (
            <TraceRow
              key={`${step.scope}-${step.rule_id}-${index}`}
              step={step}
              index={index + 1}
              defaultPolicy={result.default_policy}
            />
          ))}
        </ol>
      </div>

      <p className="text-ink-3 text-[11px] leading-[1.6]">{t('admin.rules.traceFootnote')}</p>
    </div>
  )
}

function TraceRow({
  step,
  index,
  defaultPolicy,
}: {
  step: RuleTraceStep
  index: number
  defaultPolicy: string
}) {
  const { t } = useTranslation()
  return (
    <li className="border-border flex items-center gap-2.5 border-b py-[9px] last:border-b-0">
      <span className="text-ink-3 w-4 shrink-0 font-mono text-[11px]">{index}</span>
      <span className="text-ink-3 shrink-0 text-[10.5px] tracking-[0.06em]">
        {t(`admin.rules.scope.${step.scope}`)}
      </span>
      {/* 默认策略没有 pattern——它是上游上的一个字段，不是一条规则。 */}
      <span className="text-foreground min-w-0 flex-1 truncate font-mono text-[12px]">
        {step.pattern || t(`admin.rules.policy.${defaultPolicy}`)}
      </span>
      <span
        className={cn(
          'shrink-0 text-[11px]',
          step.decisive
            ? step.action === 'deny'
              ? 'text-destructive font-semibold'
              : 'text-ok font-semibold'
            : 'text-ink-3'
        )}
      >
        {step.decisive
          ? t('admin.rules.stepDecisive', { action: t(`admin.rules.action.${step.action}`) })
          : t(step.matched ? 'admin.rules.stepMatched' : 'admin.rules.stepUnmatched')}
      </span>
    </li>
  )
}
