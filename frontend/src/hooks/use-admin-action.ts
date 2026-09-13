import { useCallback, useEffect, useRef, useState } from 'react'

import type { MutationResult } from '@/lib/api'
import { failureMessageKey } from '@/lib/errors'

export interface AdminAction {
  /** 正在写，用来把按钮压住，而不是弹一条「正在保存」的横幅。 */
  pending: boolean
  /** 上一次失败该说哪句话的文案键，成功或还没试过时是 null。 */
  errorKey: string | null
  /** 发起一次写操作，成功时把返回的数据交给 onDone。 */
  run: <T>(call: () => Promise<MutationResult<T>>, onDone?: (data: T) => void) => Promise<void>
  /** 用户又动手了：把上一次的错误收掉，别让它挂在那里像是这一次也错了。 */
  clearError: () => void
}

/**
 * 管理端写操作的统一出口。
 *
 * 三件事在这里只做一遍：密钥失效退回登录（那是会话的事，不是这一次改动的事）、
 * 业务失败翻成界面自己的文案（后端的 msg 不往外贴）、以及把按钮压住。每个屏幕
 * 各写一遍的话，总有一处会把后端的英文串直接 setState 上去。
 */
export function useAdminAction(onUnauthorized: () => void): AdminAction {
  const [pending, setPending] = useState(false)
  const [errorKey, setErrorKey] = useState<string | null>(null)
  // 回调的身份每次渲染都可能变，存在 ref 里 run 才能保持稳定，不至于让调用方的
  // useEffect 依赖跟着抖。写 ref 放在 effect 里：渲染期间改 ref 在并发渲染下
  // 会把一次被丢弃的渲染的值留下来。
  const reject = useRef(onUnauthorized)
  useEffect(() => {
    reject.current = onUnauthorized
  }, [onUnauthorized])

  const run = useCallback(
    async <T>(call: () => Promise<MutationResult<T>>, onDone?: (data: T) => void) => {
      setPending(true)
      setErrorKey(null)
      const result = await call()
      setPending(false)
      if (!result.ok && result.reason === 'unauthorized') {
        reject.current()
        return
      }
      const key = failureMessageKey(result)
      if (key) {
        setErrorKey(key)
        return
      }
      if (result.ok) {
        onDone?.(result.data)
      }
    },
    []
  )

  const clearError = useCallback(() => setErrorKey(null), [])

  return { pending, errorKey, run, clearError }
}
