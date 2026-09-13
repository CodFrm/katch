import { useCallback, useState } from 'react'

import { ADMIN_KEY_STORAGE, fetchAdminUpstreams, type AdminFailure } from '@/lib/api'

export interface AdminSession {
  /** 当前生效的管理密钥，没登录时是 null。 */
  key: string | null
  /** 上一次尝试失败的原因，成功或还没试过时是 null。 */
  failure: AdminFailure | null
  /** 正在校验，用来把按钮压住，而不是弹一条「正在校验」的横幅。 */
  verifying: boolean
  signIn: (key: string) => Promise<void>
  signOut: () => void
  /** 密钥在用的过程中失效了（被轮换掉）：清掉它并说明原因。 */
  reject: (reason: AdminFailure) => void
  /**
   * 换上一把刚生效的密钥。
   *
   * 轮换是从后台里发起的：换成功的那一刻，浏览器手上这把当场作废，下一个请求就
   * 会 401 把人踢回登录页。所以轮换成功要立刻把新的那把接过来——让发起轮换的人
   * 被自己的操作踢出去，是一种明明知道会发生却不处理的坏掉。
   */
  adopt: (key: string) => void
}

/**
 * 管理密钥的持有者。
 *
 * 密钥存在 localStorage 里：这一轮没有会话与 Cookie，密钥就是凭据，刷新一次就得
 * 重新输入的后台没人愿意用。退出登录要真的把它删掉——留在本地的凭据等于没退。
 *
 * 校验的方式是拿它去打一个管理接口。没有单独的「登录」端点，也不该有：能不能用
 * 这把密钥读到管理数据，就是它有没有效的唯一定义。
 */
export function useAdminSession(): AdminSession {
  const [key, setKey] = useState<string | null>(() => localStorage.getItem(ADMIN_KEY_STORAGE))
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [verifying, setVerifying] = useState(false)

  const signIn = useCallback(async (candidate: string) => {
    setFailure(null)
    setVerifying(true)
    const result = await fetchAdminUpstreams(candidate)
    setVerifying(false)
    if (!result.ok) {
      setFailure(result.reason)
      return
    }
    localStorage.setItem(ADMIN_KEY_STORAGE, candidate)
    setKey(candidate)
  }, [])

  const signOut = useCallback(() => {
    localStorage.removeItem(ADMIN_KEY_STORAGE)
    setKey(null)
    setFailure(null)
  }, [])

  const adopt = useCallback((next: string) => {
    localStorage.setItem(ADMIN_KEY_STORAGE, next)
    setKey(next)
    setFailure(null)
  }, [])

  const reject = useCallback((reason: AdminFailure) => {
    localStorage.removeItem(ADMIN_KEY_STORAGE)
    setKey(null)
    setFailure(reason)
  }, [])

  return { key, failure, verifying, signIn, signOut, reject, adopt }
}
