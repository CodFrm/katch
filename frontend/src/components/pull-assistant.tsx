import { CornerDownRight, Settings2 } from 'lucide-react'
import { useMemo, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Input } from '@/components/ui/input'
import { useCopy } from '@/hooks/use-copy'
import type { UpstreamItem } from '@/lib/api'
import { formatBytes } from '@/lib/format'
import { parseReference, type UpstreamRef } from '@/lib/reference'

/**
 * 拉取助手：整页由这一个输入框驱动。
 *
 * 识别是实时的——使用者不必先按什么才知道自己写对没有。回车留给「复制命令」，
 * 因为那是写完之后唯一还要做的事。
 */
export function PullAssistant({
  upstreams,
  origin,
  ready,
}: {
  upstreams: UpstreamItem[] | null
  origin: string
  ready: boolean
}) {
  const { t } = useTranslation()
  const [input, setInput] = useState('')
  const [prefixOpen, setPrefixOpen] = useState(false)

  const refs = useMemo<UpstreamRef[] | null>(
    () =>
      upstreams?.map((u) => ({
        host: u.host,
        protocols: u.protocols,
        libraryCompletion: u.library_completion,
      })) ?? null,
    [upstreams]
  )
  const parsed = useMemo(
    () => parseReference(input, { upstreams: refs, origin }),
    [input, refs, origin]
  )
  const matched =
    parsed.status === 'ok' ? upstreams?.find((u) => u.host === parsed.host) : undefined

  const command = parsed.status === 'ok' ? parsed.command : ''
  const { copied, copy } = useCopy(command)
  const prefix = parsed.status === 'ok' ? parsed.prefix : ''
  const prefixCopy = useCopy(prefix)

  function onSubmit(event: FormEvent) {
    event.preventDefault()
    if (command) {
      copy()
    }
  }

  const cached = matched && matched.cache_bytes > 0 ? formatBytes(matched.cache_bytes) : null

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <p className="text-muted-foreground text-sm">{t('assistant.guide')}</p>

      <form onSubmit={onSubmit} className="mt-5">
        <div className="border-foreground bg-background flex items-center gap-3.5 border-2 px-[22px] py-5">
          <Input
            aria-label={t('assistant.inputLabel')}
            placeholder={t('assistant.placeholder')}
            value={input}
            spellCheck={false}
            autoComplete="off"
            onChange={(event) => setInput(event.target.value)}
            className="text-foreground placeholder:text-ink-3 h-auto min-w-0 flex-1 rounded-none border-0 bg-transparent px-0 py-0 font-mono text-[26px] tracking-[-0.02em] shadow-none focus-visible:border-transparent focus-visible:ring-0 md:text-[26px] dark:bg-transparent"
          />
          <span className="text-ink-3 shrink-0 text-xs">{t('assistant.submitHint')}</span>
        </div>
      </form>

      {ready && parsed.status === 'unknown' && (
        <p className="text-warn mt-5 flex items-center gap-2 text-[13px]">
          <CornerDownRight className="text-ink-3 size-3.5 shrink-0" aria-hidden="true" />
          {t('assistant.unknown', { host: parsed.host })}
        </p>
      )}

      {ready && parsed.status === 'ok' && (
        <>
          <p className="mt-5 flex flex-wrap items-center gap-2 text-[13px]">
            <CornerDownRight className="text-ink-3 size-3.5 shrink-0" aria-hidden="true" />
            <span className="text-ink-3">{t('assistant.recognized')}</span>
            <span className="text-foreground font-mono break-all">{parsed.target}</span>
            {parsed.verified && (
              <span className="text-ink-3">
                {[
                  t(`protocol.${parsed.kind}`),
                  cached ? t('assistant.cached', { size: `${cached.value} ${cached.unit}` }) : null,
                ]
                  .filter(Boolean)
                  .map((part) => `· ${part}`)
                  .join(' ')}
              </span>
            )}
          </p>

          <div className="bg-muted border-primary mt-7 border-l-[3px] px-[22px] py-[18px]">
            <div className="flex items-center justify-between gap-4">
              <span className="text-ink-3 text-[11px] tracking-[0.13em]">
                {t('assistant.commandLabel')}
              </span>
              <CopyButton copied={copied} onCopy={copy} />
            </div>
            <p className="text-foreground mt-3 font-mono text-base leading-[1.4] break-all">
              {parsed.command}
            </p>
          </div>

          <div className="border-border mt-3.5 border">
            <div className="flex items-center gap-2.5 px-[22px] py-3">
              <Settings2 className="text-ink-3 size-3.5 shrink-0" aria-hidden="true" />
              <span className="text-muted-foreground text-[13px]">{t('assistant.persistent')}</span>
              <button
                type="button"
                onClick={() => setPrefixOpen((open) => !open)}
                aria-expanded={prefixOpen}
                className="text-primary ml-auto shrink-0 cursor-pointer text-[13px]"
              >
                {prefixOpen ? t('assistant.persistentHide') : t('assistant.persistentShow')}
              </button>
            </div>
            {prefixOpen && (
              <div className="border-border flex items-center justify-between gap-4 border-t px-[22px] py-3">
                <span className="text-ink-3 shrink-0 text-[11px] tracking-[0.13em]">
                  {t('assistant.persistentLabel')}
                </span>
                <code className="text-foreground min-w-0 flex-1 font-mono text-[13px] break-all">
                  {parsed.prefix}
                </code>
                <CopyButton copied={prefixCopy.copied} onCopy={prefixCopy.copy} />
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
