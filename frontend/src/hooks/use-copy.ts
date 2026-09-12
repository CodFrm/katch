import { useCallback, useEffect, useState } from 'react'

/** 复制成功后这个状态自己退回去，不需要使用者再点一次。 */
const RESET_MS = 2000

/**
 * 把一段文本复制到剪贴板，并把「刚刚复制过」这件事交给调用方渲染。
 *
 * 记的是「复制过的那段文本」而不是一个布尔量：使用者改了输入之后，命令就不是
 * 刚才复制的那条了，按钮必须立刻退回「复制」——用同一份 state 推出来，比再加
 * 一个副作用去清它更不容易错。
 *
 * 复制成功与否用按钮自己的形态表达，不另外弹横幅——那属于解释性提示。剪贴板
 * 不可用时什么都不做：那种环境里弹一句「复制失败」也帮不上忙。
 */
export function useCopy(value: string): { copied: boolean; copy: () => void } {
  const [copiedValue, setCopiedValue] = useState<string | null>(null)
  const copied = copiedValue !== null && copiedValue === value

  useEffect(() => {
    if (!copied) {
      return
    }
    const timer = window.setTimeout(() => setCopiedValue(null), RESET_MS)
    return () => window.clearTimeout(timer)
  }, [copied])

  const copy = useCallback(() => {
    const clipboard = navigator.clipboard
    if (!clipboard) {
      return
    }
    void clipboard.writeText(value).then(
      () => setCopiedValue(value),
      () => setCopiedValue(null)
    )
  }, [value])

  return { copied, copy }
}
