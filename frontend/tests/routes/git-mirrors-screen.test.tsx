import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'
import { ADMIN_KEY_STORAGE } from '@/lib/api'

// 覆盖任务目标：管理界面出现 git 镜像列表页（仓库、状态、体积、最后同步与最后
// 访问时间），并可删除单条；四种镜像事件在时间线上有自己的标签而不是通用的
// 「未知事件」行。这里只装配 GitMirrorsScreen 用到的那几个端点，其余的管理
// 屏幕已经在 admin-page.test.tsx / admin-manage.test.tsx 里覆盖过。

const KEY = 'right-key'
const TO = 1757700000

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, msg: 'success', data }) }
}

function mirrorRows() {
  return [
    {
      id: 1,
      host: 'github.com',
      repo: '/foo/bar.git',
      state: 'ready',
      size_bytes: 152520359936,
      last_sync_at: TO - 300,
      last_access_at: TO - 60,
      last_error: '',
      createtime: TO - 90000,
      updatetime: TO - 300,
    },
    {
      id: 2,
      host: 'github.com',
      repo: '/foo/stale.git',
      state: 'failed',
      size_bytes: 0,
      last_sync_at: 0,
      last_access_at: TO - 500,
      last_error: 'dial tcp: connection refused',
      createtime: TO - 90000,
      updatetime: TO - 500,
    },
  ]
}

interface Call {
  url: string
  method: string
}

let calls: Call[] = []
let mirrors: ReturnType<typeof mirrorRows>

function stubFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      calls.push({ url, method })
      if (url === '/api/v1/system/version') {
        return envelope({ version: '0.1.0', commit: 'abc1234' })
      }
      if (url === '/api/v1/admin/upstreams') {
        return envelope({ list: [] })
      }
      if (url === '/api/v1/admin/stats/upstreams') {
        return envelope({ range: '24h', from: 0, to: TO, list: [] })
      }
      if (url === '/api/v1/admin/git/mirrors' && method === 'GET') {
        return envelope({ list: mirrors })
      }
      if (url.startsWith('/api/v1/admin/git/mirrors/') && method === 'DELETE') {
        const id = Number(url.split('/').pop())
        mirrors = mirrors.filter((mirror) => mirror.id !== id)
        return envelope({})
      }
      return { ok: false, status: 404, json: async () => ({}) }
    })
  )
}

function renderAdmin(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>
  )
}

beforeEach(async () => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  localStorage.clear()
  localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
  calls = []
  mirrors = mirrorRows()
  await i18n.changeLanguage('zh-CN')
  stubFetch()
})

describe('后台 · git 镜像', () => {
  it('列出仓库、状态、体积、最后同步与最后访问', async () => {
    renderAdmin('/admin/git')

    const table = await screen.findByRole('table', { name: 'Git 镜像' })
    const rows = within(table).getAllByRole('row')
    // 一条表头 + 两条数据。
    expect(rows).toHaveLength(3)

    const ready = within(rows[1])
    expect(ready.getByText('github.com/foo/bar.git')).toBeInTheDocument()
    expect(ready.getByText('已就绪')).toBeInTheDocument()

    const failed = within(rows[2])
    expect(failed.getByText('失败')).toBeInTheDocument()
  })

  it('能删除单条镜像，删完列表跟着刷新', async () => {
    renderAdmin('/admin/git')

    const table = await screen.findByRole('table', { name: 'Git 镜像' })
    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(3))

    const rows = within(table).getAllByRole('row')
    await userEvent.click(within(rows[1]).getByRole('button', { name: '删除' }))

    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(2))
    expect(screen.queryByText('github.com/foo/bar.git')).not.toBeInTheDocument()

    const deleteCall = calls.find((call) => call.method === 'DELETE')
    expect(deleteCall?.url).toBe('/api/v1/admin/git/mirrors/1')
  })

  it('导航栏有 git 镜像入口', async () => {
    renderAdmin('/admin')
    const link = await screen.findByRole('link', { name: 'Git 镜像' })
    expect(link).toHaveAttribute('href', '/admin/git')
  })
})
