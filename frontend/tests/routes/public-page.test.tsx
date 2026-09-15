import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'
import type { Overview, UpstreamList, VersionInfo } from '@/lib/api'

const upstreams: UpstreamList = {
  list: [
    {
      host: 'docker.io',
      protocols: ['registry'],
      library_completion: true,
      hit_rate: 0.942,
      cache_bytes: 871938031616,
      status: 'normal',
    },
    {
      host: 'deb.debian.org',
      protocols: ['static'],
      library_completion: false,
      hit_rate: 0.961,
      cache_bytes: 462754185216,
      status: 'degraded',
    },
  ],
}

const overview: Overview = {
  range: '30d',
  from: 0,
  to: 0,
  requests: 1000,
  hits: 900,
  denied: 10,
  origin_errors: 5,
  bytes_served: 0,
  bytes_origin: 0,
  cache_bytes: 2001454279065,
  daily: Array.from({ length: 14 }, (_, i) => ({
    day: i,
    requests: 100,
    hits: 80 + i,
    bytes_served: i === 13 ? 500 : 0,
    bytes_origin: i === 13 ? 160 : 0,
  })),
}

const version: VersionInfo = { version: '0.1.0', commit: 'abc1234' }

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, msg: 'success', data }) }
}

const notFound = { ok: false, status: 404, json: async () => ({}) }

function stubFetch(routes: Record<string, unknown>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string) => {
      for (const [prefix, response] of Object.entries(routes)) {
        if (input.startsWith(prefix)) {
          return response
        }
      }
      return notFound
    })
  )
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/']}>
      <App />
    </MemoryRouter>
  )
}

const allPublic = {
  '/api/v1/upstreams': envelope(upstreams),
  '/api/v1/stats/overview': envelope(overview),
  '/api/v1/system/version': envelope(version),
}

describe('拉取助手', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    await i18n.changeLanguage('zh-CN')
  })

  it('输入 redis:7 渲染出识别到的上游与可复制的拉取命令', async () => {
    stubFetch(allPublic)
    renderPage()
    await screen.findByText('docker.io')

    await userEvent.type(screen.getByRole('textbox', { name: '要拉什么' }), 'redis:7')

    expect(await screen.findByText('docker.io/library/redis:7')).toBeInTheDocument()
    expect(
      screen.getByText('docker pull localhost:3000/docker.io/library/redis:7')
    ).toBeInTheDocument()
  })

  it('复制按钮把命令原样写进剪贴板', async () => {
    stubFetch(allPublic)
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    renderPage()

    await userEvent.type(screen.getByRole('textbox', { name: '要拉什么' }), 'redis:7')
    await userEvent.click(await screen.findByRole('button', { name: '复制' }))

    expect(writeText).toHaveBeenCalledWith('docker pull localhost:3000/docker.io/library/redis:7')
    expect(await screen.findByRole('button', { name: '已复制' })).toBeInTheDocument()
  })

  it('上游表里没有的主机不拼命令，只说没收录', async () => {
    stubFetch(allPublic)
    renderPage()
    await screen.findByText('docker.io')

    await userEvent.type(screen.getByRole('textbox', { name: '要拉什么' }), 'quay.io/foo/bar:1')

    expect(await screen.findByText(/quay\.io/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '复制' })).not.toBeInTheDocument()
  })
})

describe('侧栏指标', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    await i18n.changeLanguage('zh-CN')
  })

  it('命中率、缓存量与省下的回源流量都来自公开统计接口', async () => {
    stubFetch(allPublic)
    renderPage()

    // 1000 次请求里 10 次被拒、5 次回源失败，问过缓存的是 985 次，命中 900 次。
    expect(await screen.findByText('91.4')).toBeInTheDocument()
    expect(screen.getByText('1.82')).toBeInTheDocument()
    expect(screen.getByText('TB')).toBeInTheDocument()
    expect(screen.getByText('340')).toBeInTheDocument()
  })

  it('近 14 天趋势按逐日序列画满 14 根柱子', async () => {
    stubFetch(allPublic)
    renderPage()

    const trend = await screen.findByRole('img', { name: '近 14 天命中率趋势' })
    expect(trend.querySelectorAll('[data-slot="trend-bar"]')).toHaveLength(14)
  })
})

describe('支持的上游', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    await i18n.changeLanguage('zh-CN')
  })

  it('页脚按接口给的顺序列出上游、类型、命中率、缓存量与状态', async () => {
    stubFetch(allPublic)
    renderPage()

    const rows = await screen.findAllByRole('row')
    const body = rows.slice(1)
    expect(body).toHaveLength(2)
    expect(body[0]).toHaveTextContent('docker.io')
    expect(body[0]).toHaveTextContent('容器镜像')
    expect(body[0]).toHaveTextContent('94.2%')
    expect(body[0]).toHaveTextContent('812 GB')
    expect(body[0]).toHaveTextContent('正常')
    expect(body[1]).toHaveTextContent('deb.debian.org')
    expect(body[1]).toHaveTextContent('静态资源')
    expect(body[1]).toHaveTextContent('限流中')
  })

  it('只渲染接口返回的两种状态，不臆造第三种', async () => {
    stubFetch(allPublic)
    renderPage()
    await screen.findByText('deb.debian.org')

    expect(screen.getByText('正常')).toBeInTheDocument()
    expect(screen.getByText('限流中')).toBeInTheDocument()
  })
})

// 站长把「公开首页」关掉后，匿名调用方两个接口都拿 404。首页不能因此变成一块
// 错误提示：它首先是个拉取助手，统计和上游表只是名片。
describe('公开数据被关掉时', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    await i18n.changeLanguage('zh-CN')
  })

  it('助手照常识别并给出命令，侧栏与页脚整块消失且不报错', async () => {
    stubFetch({ '/api/v1/system/version': envelope(version) })
    renderPage()

    await userEvent.type(screen.getByRole('textbox', { name: '要拉什么' }), 'redis:7')

    expect(
      await screen.findByText('docker pull localhost:3000/docker.io/library/redis:7')
    ).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.queryByText('近 30 天命中率')).not.toBeInTheDocument()
    expect(screen.queryByText(/404/)).not.toBeInTheDocument()
  })

  // 上游表拿不到就没有据可依，此时说「这是个容器镜像、已缓存 X」是在替后端
  // 断言一件没核对过的事。命令照给，多余的断言不给。
  it('不声称没核对过的类别与缓存量', async () => {
    stubFetch({ '/api/v1/system/version': envelope(version) })
    renderPage()

    await userEvent.type(screen.getByRole('textbox', { name: '要拉什么' }), 'redis:7')

    expect(await screen.findByText('docker.io/library/redis:7')).toBeInTheDocument()
    expect(screen.queryByText(/容器镜像/)).not.toBeInTheDocument()
    expect(screen.queryByText(/已缓存/)).not.toBeInTheDocument()
  })
})

describe('两个面的外壳', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    await i18n.changeLanguage('zh-CN')
  })

  it('/admin 走后台那一面，不渲染前台的拉取助手', async () => {
    stubFetch(allPublic)
    render(
      <MemoryRouter initialEntries={['/admin']}>
        <App />
      </MemoryRouter>
    )

    // 后台那一面从登录开始：密钥握在浏览器里，没有它就只有这一屏。
    expect(await screen.findByRole('heading', { name: '管理登录' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '要拉什么' })).not.toBeInTheDocument()
  })

  it('认不出来的路径回到前台', async () => {
    stubFetch(allPublic)
    render(
      <MemoryRouter initialEntries={['/nope']}>
        <App />
      </MemoryRouter>
    )

    expect(await screen.findByRole('textbox', { name: '要拉什么' })).toBeInTheDocument()
  })
})
