/**
 * 把后端的业务错误码翻成界面自己的文案键。
 *
 * 后端的 msg 是给日志和接口调用方的，不是给用户的：它有自己的措辞、有格式化过的
 * 键名，而且只注册了中文一种语言。所以界面按**码**分支，话由 locales 里的文案说。
 * 认不出来的码也必须有出路——后端将来加了新码，界面宁可说一句「这次改动没能保存」，
 * 也不能把 `10007` 或一串英文贴给用户。
 */

import type { MutationResult } from './api'

/** 后端 internal/pkg/code 里的业务码。只列界面真会分支的那几个。 */
const MESSAGES: Record<number, string> = {
  10002: 'admin.error.upstreamHostExists',
  10004: 'admin.error.ruleNotFound',
  10005: 'admin.error.cacheObjectNotFound',
  10006: 'admin.error.purgeTargetRequired',
  10007: 'admin.error.settingKeyUnknown',
  10008: 'admin.error.settingValueInvalid',
}

/**
 * 一次失败该说哪句话，成功时给 null。
 *
 * unauthorized 不在这里：密钥失效要做的是把人退回登录页（见 useAdminSession），
 * 在原地留一句红字等于把人晾在一个已经不能用的会话里。
 */
export function failureMessageKey<T>(result: MutationResult<T>): string | null {
  if (result.ok) {
    return null
  }
  if (result.reason === 'unreachable') {
    return 'admin.error.unreachable'
  }
  if (result.reason === 'rejected') {
    return MESSAGES[result.code] ?? 'admin.error.rejected'
  }
  return null
}
