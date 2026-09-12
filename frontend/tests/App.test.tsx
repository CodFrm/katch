import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'

describe('App', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    // 固定语言：断言的是「错误态渲染了 system.error」这条行为，
    // 不该因为运行环境的语言检测结果而飘。
    await i18n.changeLanguage('zh-CN')
  })

  it('拿到版本信息后把它显示出来', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ version: '1.2.3', commit: 'abc1234' }),
      })
    )
    render(<App />)
    await waitFor(() => {
      expect(screen.getByText(/1\.2\.3/)).toBeInTheDocument()
    })
    expect(screen.getByText(/abc1234/)).toBeInTheDocument()
  })

  // 接口挂掉时必须给出可见的失败提示。如果这里退化成一直显示「加载中」，
  // 用户会以为是网络慢而一直等下去，问题被一个看起来正常的状态藏住。
  it('接口失败时显示错误文案而不是停在加载中', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('boom')))
    render(<App />)
    await waitFor(() => {
      expect(screen.getByText('无法获取版本信息')).toBeInTheDocument()
    })
  })
})
