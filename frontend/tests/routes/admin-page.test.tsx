import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'
import { ADMIN_KEY_STORAGE } from '@/lib/api'

const KEY = 'right-key'
const HOUR = 3600
const TO = 1757700000

const upstreams = {
  list: [
    {
      id: 1,
      host: 'docker.io',
      kind: 'registry',
      origin: 'https://registry-1.docker.io',
      enabled: true,
      immutable_patterns: ['@sha256:'],
      mutable_ttl_seconds: 300,
      default_policy: 'allow_all',
      library_completion: true,
      note: '',
      createtime: 0,
      updatetime: 0,
    },
    {
      id: 2,
      host: 'pypi.org',
      kind: 'static',
      origin: 'https://pypi.org',
      enabled: true,
      immutable_patterns: [],
      mutable_ttl_seconds: 600,
      default_policy: 'allow_all',
      library_completion: false,
      note: '',
      createtime: 0,
      updatetime: 0,
    },
  ],
}

const stats = {
  range: '24h',
  from: TO - 24 * HOUR,
  to: TO,
  list: [
    {
      upstream_id: 1,
      host: 'docker.io',
      requests: 1000,
      hits: 942,
      denied: 0,
      origin_errors: 0,
      bytes_served: 3221225472,
      bytes_origin: 1073741824,
      degraded: false,
      retry_at: 0,
    },
    {
      upstream_id: 2,
      host: 'pypi.org',
      requests: 200,
      hits: 100,
      denied: 0,
      origin_errors: 20,
      bytes_served: 1073741824,
      bytes_origin: 1073741824,
      degraded: true,
      retry_at: TO + 300,
    },
  ],
}

const overview = {
  range: '24h',
  from: TO - 24 * HOUR,
  to: TO,
  requests: 1200,
  hits: 1042,
  denied: 0,
  origin_errors: 20,
  bytes_served: 4294967296,
  bytes_origin: 2147483648,
  cache_bytes: 2001454279065,
  daily: [],
}

function series(upstreamID: number) {
  return {
    range: '24h',
    from: TO - 24 * HOUR,
    to: TO,
    bucket_seconds: HOUR,
    list: Array.from({ length: 24 }, (_, i) => {
      const scale = (i + 1) * upstreamID
      // 未命中数正好等于四个原因之和：后端的口径就是这样，用例里对不上的话
      // 占比条和堆叠图会各说各的。
      return {
        bucket: TO - (24 - i) * HOUR,
        requests: 50 * scale,
        hits: 40 * scale,
        denied: 0,
        origin_errors: 0,
        bytes_served: 0,
        bytes_origin: 0,
        miss_first: 6 * scale,
        miss_ttl: 2 * scale,
        miss_evicted: scale,
        miss_changed: scale,
      }
    }),
  }
}

/** 一段一次回源都没有的序列：全是命中，四个原因都是零。 */
function quietSeries() {
  return {
    range: '24h',
    from: TO - 24 * HOUR,
    to: TO,
    bucket_seconds: HOUR,
    list: Array.from({ length: 24 }, (_, i) => ({
      bucket: TO - (24 - i) * HOUR,
      requests: 8,
      hits: 8,
      denied: 0,
      origin_errors: 0,
      bytes_served: 0,
      bytes_origin: 0,
      miss_first: 0,
      miss_ttl: 0,
      miss_evicted: 0,
      miss_changed: 0,
    })),
  }
}

const events = {
  list: [
    {
      id: 9,
      kind: 'upstream_degraded',
      actor: 'system',
      upstream_id: 2,
      detail: { host: 'pypi.org' },
      createtime: TO - 600,
    },
    {
      id: 8,
      kind: 'rule_created',
      actor: 'admin',
      upstream_id: 1,
      detail: { rule_id: 3, action: 'deny', pattern: 'library/alpine:3.21' },
      createtime: TO - 4000,
    },
    {
      id: 7,
      kind: 'cache_reclaimed',
      actor: 'system',
      upstream_id: 0,
      detail: { removed: 142, freed_bytes: 152520359936, quota_bytes: 3298534883328 },
      createtime: TO - 9000,
    },
    {
      id: 6,
      kind: 'upstream_created',
      actor: 'admin',
      upstream_id: 2,
      detail: { host: 'pypi.org', kind: 'static', origin: 'https://pypi.org', enabled: true },
      createtime: TO - 90000,
    },
  ],
}

/**
 * 最近请求：后端从 recent_request 这张明细表读回来的几行，最近的在最前。
 *
 * 这块面板的数据来自库（决策 2/11），所以「读不到」是它的常态之一：库里还没有
 * 这个上游的行时，后端给一个空列表。
 */
const recent = {
  list: [
    { at: TO - 5, object: 'library/redis:7', result: 'hit', bytes: 43200000, duration_ms: 900 },
    {
      at: TO - 17,
      object: 'library/nginx:1.27',
      result: 'miss',
      bytes: 71700000,
      duration_ms: 3200,
    },
    { at: TO - 60, object: 'library/alpine:3.21', result: 'denied', bytes: 0, duration_ms: 0 },
    { at: TO - 90, object: 'library/mysql:8', result: 'origin_error', bytes: 0, duration_ms: 120 },
  ],
}

/** 后端的 401：cago 的信封 + 一句英文串，界面一个字都不该贴出来。 */
const unauthorized = {
  ok: false,
  status: 401,
  json: async () => ({ code: 401, msg: 'Unauthorized: admin key invalid', data: null }),
}

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, msg: 'success', data }) }
}

let requests: { url: string; key: string | null }[] = []

function stubFetch(
  options: { key?: string; series?: (id: number) => unknown; recent?: (id: number) => unknown } = {}
) {
  const good = options.key ?? KEY
  const seriesOf = options.series ?? series
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const header = new Headers(init?.headers).get('Authorization')
      requests.push({ url, key: header })
      if (url.startsWith('/api/v1/system/version')) {
        return envelope({ version: '0.1.0', commit: 'abc1234' })
      }
      if (header !== `Bearer ${good}`) {
        return unauthorized
      }
      if (url.startsWith('/api/v1/admin/logs/requests')) {
        const id = Number(new URLSearchParams(url.split('?')[1]).get('upstream_id'))
        // 没给就当日志读不到：后端在没开落盘、文件被轮转走时给的就是空列表。
        return envelope(options.recent?.(id) ?? { list: [] })
      }
      if (url.startsWith('/api/v1/admin/stats/upstreams/series')) {
        const id = Number(new URLSearchParams(url.split('?')[1]).get('upstream_id'))
        return envelope(seriesOf(id))
      }
      if (url.startsWith('/api/v1/admin/stats/upstreams')) {
        return envelope(stats)
      }
      if (url.startsWith('/api/v1/admin/upstreams')) {
        return envelope(upstreams)
      }
      if (url.startsWith('/api/v1/admin/events')) {
        return envelope(events)
      }
      if (url.startsWith('/api/v1/stats/overview')) {
        return envelope(overview)
      }
      return { ok: false, status: 404, json: async () => ({}) }
    })
  )
}

function renderAdmin(path = '/admin') {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>
  )
}

async function login() {
  await userEvent.type(screen.getByLabelText('管理员密钥'), KEY)
  await userEvent.click(screen.getByRole('button', { name: '进入管理台' }))
}

describe('后台登录', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    localStorage.clear()
    requests = []
    await i18n.changeLanguage('zh-CN')
  })

  it('密钥不对时给一句翻译过的说明，不贴后端的英文串', async () => {
    stubFetch()
    renderAdmin()

    await userEvent.type(screen.getByLabelText('管理员密钥'), 'wrong-key')
    await userEvent.click(screen.getByRole('button', { name: '进入管理台' }))

    const failure = await screen.findByRole('alert')
    expect(failure).toHaveTextContent('这把密钥进不去')
    expect(screen.queryByText(/Unauthorized/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/admin key invalid/i)).not.toBeInTheDocument()
    // 进不去就还在登录页，密钥也不该被留在本地。
    expect(screen.getByLabelText('管理员密钥')).toBeInTheDocument()
    expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBeNull()
  })

  it('连不上后端时说的是另一件事，不是密钥不对', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('Failed to fetch')
      })
    )
    renderAdmin()
    await login()

    const failure = await screen.findByRole('alert')
    expect(failure).toHaveTextContent('连不上')
    expect(failure).not.toHaveTextContent('这把密钥进不去')
  })

  it('密钥有效时存下来，刷新后直接进概览', async () => {
    stubFetch()
    const first = renderAdmin()
    await login()

    expect(await screen.findByRole('heading', { name: '概览' })).toBeInTheDocument()
    expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBe(KEY)

    // 重新挂载 = 刷新页面：不再问一次密钥。
    first.unmount()
    const second = renderAdmin()
    expect(await second.findByRole('heading', { name: '概览' })).toBeInTheDocument()
    expect(second.queryByLabelText('管理员密钥')).not.toBeInTheDocument()
  })

  it('退出登录真的把密钥清掉', async () => {
    stubFetch()
    localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
    renderAdmin()

    await userEvent.click(await screen.findByRole('button', { name: '退出登录' }))

    expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBeNull()
    expect(await screen.findByLabelText('管理员密钥')).toBeInTheDocument()
  })

  it('存着的密钥已经失效时退回登录页，并说明原因', async () => {
    stubFetch({ key: 'rotated-key' })
    localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
    renderAdmin()

    expect(await screen.findByRole('alert')).toHaveTextContent('这把密钥进不去')
    expect(screen.getByLabelText('管理员密钥')).toBeInTheDocument()
    expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBeNull()
  })
})

describe('后台概览', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    localStorage.clear()
    localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
    requests = []
    await i18n.changeLanguage('zh-CN')
  })

  it('每个上游一块：主机名、命中率、状态点与近 12 小时的迷你柱', async () => {
    stubFetch()
    renderAdmin()

    const matrix = await screen.findByRole('list', { name: '上游健康' })
    const cells = within(matrix).getAllByRole('listitem')
    expect(cells).toHaveLength(2)
    expect(cells[0]).toHaveTextContent('docker.io')
    expect(cells[0]).toHaveTextContent('94.2')
    expect(cells[1]).toHaveTextContent('pypi.org')
    expect(cells[1]).toHaveTextContent('限流中')
    expect(cells[0].querySelectorAll('[data-slot="mini-bar"]')).toHaveLength(12)
  })

  it('指标条按区间合计，错误率指出错得最多的那个上游', async () => {
    stubFetch()
    renderAdmin()

    const bar = await screen.findByRole('group', { name: '全局指标' })
    // 问过缓存的是 1200-20=1180 次，命中 1042 次：88.3%；错误 20 次，错误率 1.7%。
    expect(bar).toHaveTextContent('88.3')
    expect(bar).toHaveTextContent('1.7')
    expect(bar).toHaveTextContent('主要来自 pypi.org')
    // 缓存占用来自总览接口。
    expect(bar).toHaveTextContent('1.82')
  })

  it('事件流把 kind 与 actor 翻成中文，不出现后端枚举', async () => {
    stubFetch()
    renderAdmin()

    const stream = await screen.findByRole('table', { name: '最近事件' })
    const rows = within(stream).getAllByRole('row')
    expect(rows[0]).toHaveTextContent('pypi.org 回源连续失败，已进入退避')
    expect(rows[0]).toHaveTextContent('降级')
    expect(rows[0]).toHaveTextContent('自动')
    expect(rows[1]).toHaveTextContent('新增规则 拒绝 library/alpine:3.21')
    expect(rows[1]).toHaveTextContent('管理员')
    expect(rows[2]).toHaveTextContent('淘汰 142 个冷对象，腾出 142 GB')
    expect(rows[3]).toHaveTextContent('添加上游 pypi.org')

    for (const raw of ['upstream_degraded', 'rule_created', 'cache_reclaimed', 'system', 'admin']) {
      expect(within(stream).queryByText(raw)).not.toBeInTheDocument()
    }
  })

  it('认不出来的 kind 也不把枚举贴到界面上', async () => {
    stubFetch()
    events.list.unshift({
      id: 10,
      kind: 'meteor_strike',
      actor: 'system',
      upstream_id: 0,
      detail: {},
      createtime: TO - 30,
    } as (typeof events.list)[number])
    renderAdmin()

    const stream = await screen.findByRole('table', { name: '最近事件' })
    expect(within(stream).queryByText(/meteor_strike/)).not.toBeInTheDocument()
    expect(within(stream).getAllByRole('row')[0]).toHaveTextContent('一条没见过的事件')
    events.list.shift()
  })
})

describe('后台上游详情', () => {
  beforeEach(async () => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    localStorage.clear()
    localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
    requests = []
    await i18n.changeLanguage('zh-CN')
  })

  it('点一个上游进详情：指标条与 24 小时命中/回源图', async () => {
    stubFetch()
    renderAdmin()

    const matrix = await screen.findByRole('list', { name: '上游健康' })
    await userEvent.click(within(matrix).getByRole('link', { name: /docker\.io/ }))

    expect(await screen.findByRole('heading', { name: 'docker.io' })).toBeInTheDocument()

    const bar = await screen.findByRole('group', { name: '上游指标' })
    expect(bar).toHaveTextContent('94.2')
    // 1000 次请求命中 942 次，剩下 58 次回源。
    expect(bar).toHaveTextContent('58')

    const chart = await screen.findByRole('img', { name: '最近 24 小时的命中与回源' })
    const bars = chart.querySelectorAll('[data-slot="series-bar"]')
    expect(bars).toHaveLength(24)
    // 每根柱子是「回源」叠在「命中」上，两段都要在。
    expect(bars[23].querySelector('[data-slot="series-origin"]')).not.toBeNull()
    expect(bars[23].querySelector('[data-slot="series-hit"]')).not.toBeNull()

    const seriesCall = requests.find((r) => r.url.includes('/stats/upstreams/series?'))
    expect(seriesCall?.url).toContain('upstream_id=1')
    expect(seriesCall?.key).toBe(`Bearer ${KEY}`)
  })

  it('回源原因分解成四项，占比之和正好 100%', async () => {
    // 堆叠图只说「有多少请求最后还是去了上游」，占比条回答的是下一个问题：
    // 为什么去的。这两张图读的是同一份序列，所以它们不会各说各的。
    stubFetch()
    renderAdmin('/admin/upstreams/1')

    const panel = await screen.findByRole('group', { name: '回源原因' })
    const rows = panel.querySelectorAll('[data-slot="miss-reason"]')
    expect(rows).toHaveLength(4)
    expect(
      [...rows].map((row) => row.querySelector('[data-slot="miss-label"]')?.textContent)
    ).toEqual(['首次拉取', 'TTL 过期', '缓存被淘汰', '上游 digest 变更'])

    const percents = [...rows].map((row) =>
      Number(row.querySelector('[data-slot="miss-share"]')?.textContent?.replace('%', ''))
    )
    expect(percents).toEqual([60, 20, 10, 10])
    expect(percents.reduce((sum, value) => sum + value, 0)).toBe(100)
    // 条的长度就是占比本身，不是另算一套。
    const bar = rows[0].querySelector('[data-slot="miss-bar"]') as HTMLElement
    expect(bar.style.width).toBe('60%')
  })

  it('这段时间一次回源都没有时不画四条零', async () => {
    // 四条 0% 和「全是命中」长得一样，而后者是好消息——画出来只会让人以为
    // 数据没上来。
    stubFetch({ series: () => quietSeries() })
    renderAdmin('/admin/upstreams/1')

    const panel = await screen.findByRole('group', { name: '回源原因' })
    expect(panel.querySelectorAll('[data-slot="miss-reason"]')).toHaveLength(0)
    expect(within(panel).getByText('这段时间没有回源')).toBeInTheDocument()
  })

  it('最近请求读的是库：时间、对象、结果、大小、耗时', async () => {
    // 这块面板答的是「刚刚发生了什么」——分钟级的 rollup 答不了它，所以它读的是
    // recent_request 这张每请求一行的明细表（决策 2/11）。
    stubFetch({ recent: () => recent })
    renderAdmin('/admin/upstreams/1')

    const table = await screen.findByRole('table', { name: '最近请求' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(4)

    const first = within(rows[0])
      .getAllByRole('cell')
      .map((cell) => cell.textContent)
    expect(first[1]).toBe('library/redis:7')
    expect(first[2]).toBe('命中')
    expect(first[3]).toBe('41 MB')
    expect(first[4]).toBe('900 ms')
    // 时刻精确到秒：同一分钟里的几次拉取要分得出先后。
    expect(first[0]).toMatch(/^\d{2}:\d{2}:\d{2}$/)

    // 到了一秒就写秒：毫秒数在这个量级上没人读。
    expect(within(rows[1]).getAllByRole('cell')[4]).toHaveTextContent('3.2 s')
    expect(within(rows[1]).getAllByRole('cell')[2]).toHaveTextContent('回源')
    expect(within(rows[3]).getAllByRole('cell')[2]).toHaveTextContent('回源失败')

    // 规则拒绝那一行没有字节也没有耗时，占位用破折号而不是 0 B / 0 ms——
    // 后者是「量到了，是零」，而这里根本没量。
    const denied = within(rows[2])
      .getAllByRole('cell')
      .map((cell) => cell.textContent)
    expect(denied[2]).toBe('规则拒绝')
    expect(denied[3]).toBe('—')
    expect(denied[4]).toBe('—')

    const call = requests.find((r) => r.url.startsWith('/api/v1/admin/logs/requests'))
    expect(call?.url).toContain('upstream_id=1')
    expect(call?.key).toBe(`Bearer ${KEY}`)
    // 端点不认文件名：数据不来自任何文件，界面也不带这个参数。
    expect(call?.url).not.toMatch(/file|filename|path|log=/)
  })

  it('库里没有行时这一块整个消失，而不是挂一句读不到数据', async () => {
    // 后端在库里没有这个上游的行、或库读不出来时给空列表或 503；界面据此让面板
    // 消失。排障的辅助块消失，好过让整屏管理界面挂在一句错误上。
    stubFetch({ recent: () => ({ list: [] }) })
    renderAdmin('/admin/upstreams/1')

    expect(await screen.findByRole('group', { name: '回源原因' })).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: '最近请求' })).not.toBeInTheDocument()
    expect(screen.queryByText('最近请求')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('切到另一个上游时不把上一个的最近请求留在屏幕上', async () => {
    // 手里那份行属于上一个上游。换一个上游之后它们还挂在屏幕上的话，看的人会把
    // 别人的拉取记在这个上游头上——而这块面板本来就是拿来回答「它怎么了」的。
    stubFetch({ recent: (id) => (id === 1 ? recent : { list: [] }) })
    renderAdmin('/admin/upstreams/1')
    expect(await screen.findByRole('table', { name: '最近请求' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('link', { name: /pypi\.org/ }))

    expect(await screen.findByRole('heading', { name: 'pypi.org' })).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.queryByRole('table', { name: '最近请求' })).not.toBeInTheDocument()
    )
    const calls = requests.filter((r) => r.url.startsWith('/api/v1/admin/logs/requests'))
    expect(calls.some((r) => r.url.includes('upstream_id=2'))).toBe(true)
  })

  it('详情头给的是这个上游的登记信息，不是别人的', async () => {
    stubFetch()
    renderAdmin('/admin/upstreams/2')

    expect(await screen.findByRole('heading', { name: 'pypi.org' })).toBeInTheDocument()
    expect(screen.getByText('https://pypi.org')).toBeInTheDocument()
    expect(screen.getByText('静态资源')).toBeInTheDocument()
    expect(screen.getByText('限流中')).toBeInTheDocument()
  })
})
