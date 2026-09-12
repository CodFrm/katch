import { describe, expect, it } from 'vitest'

import en from '@/i18n/locales/en.json'
import zhCN from '@/i18n/locales/zh-CN.json'

function keys(value: unknown, prefix = ''): string[] {
  if (typeof value !== 'object' || value === null) {
    return [prefix]
  }
  return Object.entries(value as Record<string, unknown>).flatMap(([k, v]) =>
    keys(v, prefix ? `${prefix}.${k}` : k)
  )
}

// 两份资源必须同进同退：漏掉的那一条不会报错，只会安静地显示成另一种语言，
// 往往要等用户反馈才被发现（docs/frontend.md#i18n）。
describe('语言资源', () => {
  it('中英两份的键完全一致', () => {
    expect(keys(en).sort()).toEqual(keys(zhCN).sort())
  })

  it('没有空文案', () => {
    const empty = Object.entries({ en, 'zh-CN': zhCN }).flatMap(([lang, res]) =>
      keys(res)
        .filter((k) => !k.split('.').reduce<unknown>((o, p) => (o as never)?.[p], res))
        .map((k) => `${lang}:${k}`)
    )
    expect(empty).toEqual([])
  })
})
