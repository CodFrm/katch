import { Check, Copy } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

/**
 * 复制按钮。
 *
 * 「复制成功」由按钮自己变成「已复制」来表达，不另开一条提示——状态用控件自身的
 * 形态说（spec 排版一节）。
 */
export function CopyButton({
  copied,
  onCopy,
  className,
}: {
  copied: boolean
  onCopy: () => void
  className?: string
}) {
  const { t } = useTranslation()
  const Icon = copied ? Check : Copy
  return (
    <button
      type="button"
      onClick={onCopy}
      className={cn(
        'flex cursor-pointer items-center gap-1.5 text-xs transition-colors',
        copied ? 'text-ok' : 'text-muted-foreground hover:text-foreground',
        className
      )}
    >
      <Icon className="size-3.5" aria-hidden="true" />
      {copied ? t('assistant.copied') : t('assistant.copy')}
    </button>
  )
}
