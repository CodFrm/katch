import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'
import { ADMIN_KEY_STORAGE } from '@/lib/api'

const KEY = 'right-key'
const HOUR = 3600
const TO = 1757700000
const GB = 1024 ** 3

/** 一次请求的记录，用来断言界面究竟发了什么给后端。 */
interface Call {
  url: string
  method: string
  key: string | null
  body: Record<string, unknown>
}

let calls: Call[] = []
let acceptedKey = KEY
/** 让某个请求悬在路上，用来造出「密钥换掉时它才回来」这一幕。 */
let holdRules: Promise<unknown> | null = null

const pendingAPTReadiness = {
  ready: true,
  missing: [],
  companions: [],
  guidance: {
    clients: ['apt'],
    configuration: [
      'deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] https://katch.dev/deb.debian.org/debian <suite> <components>',
    ],
    constraints: ['trailing_slash'],
    runtime_verified: false,
  },
}

const packageProfiles = [
  { profile: 'none', name: 'None' },
  { profile: 'apt', name: 'APT' },
  { profile: 'pypi', name: 'pypi' },
]

function upstreamRows() {
  return [
    {
      id: 1,
      host: 'docker.io',
      protocols: ['registry'],
      package_profile: 'none',
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
      host: 'deb.debian.org',
      protocols: ['static'],
      package_profile: 'apt',
      package_readiness: pendingAPTReadiness,
      origin: 'https://deb.debian.org',
      enabled: true,
      immutable_patterns: [],
      mutable_ttl_seconds: 600,
      default_policy: 'deny_unless_matched',
      library_completion: false,
      note: '',
      createtime: 0,
      updatetime: 0,
    },
  ]
}

function ruleRows() {
  return [
    {
      id: 11,
      upstream_id: 0,
      action: 'deny',
      pattern: '*:latest',
      note: '禁止浮动 tag 进生产',
      createtime: 0,
      updatetime: 0,
    },
    {
      id: 1,
      upstream_id: 1,
      action: 'allow',
      pattern: 'library/*',
      note: '官方镜像，全员可拉',
      createtime: 0,
      updatetime: 0,
    },
    {
      id: 3,
      upstream_id: 1,
      action: 'deny',
      pattern: 'library/alpine:3.21',
      note: '存在 CVE-2025-3104，禁止拉取',
      createtime: 0,
      updatetime: 0,
    },
  ]
}

function cacheRows() {
  return [
    {
      id: 101,
      upstream_id: 1,
      key: 'library/redis:7',
      digest: 'sha256:9f3a2c',
      size: 432013312,
      immutable: true,
      pinned: false,
      expires_at: 0,
      last_access_at: TO - 120,
      hit_count: 12480,
      createtime: TO - 90000,
      updatetime: TO - 120,
    },
    {
      id: 102,
      upstream_id: 2,
      key: 'pool/main/g/glibc/libc6_2.41-1_amd64.deb',
      digest: 'sha256:4b1e77',
      size: 2936012,
      immutable: true,
      pinned: true,
      expires_at: 0,
      last_access_at: TO - 60,
      hit_count: 41203,
      createtime: TO - 90000,
      updatetime: TO - 60,
    },
  ]
}

function settingRows() {
  return [
    { key: 'site_name', value: 'katch 镜像站', type: 'string' },
    { key: 'site_domain', value: 'katch.dev', type: 'string' },
    { key: 'public_homepage', value: true, type: 'bool' },
    { key: 'cache_quota_bytes', value: 3 * 1024 * GB, type: 'int' },
    { key: 'cache_reclaim_percent', value: 85, type: 'int' },
    { key: 'mutable_ttl_seconds', value: 300, type: 'int' },
    { key: 'recent_request_retention_seconds', value: 86400, type: 'int' },
    { key: 'origin_concurrency', value: 64, type: 'int' },
    { key: 'origin_timeout_seconds', value: 30, type: 'int' },
    { key: 'origin_retries', value: 2, type: 'int' },
    { key: 'git_mirror_quota_bytes', value: 10 * GB, type: 'int' },
    { key: 'git_repo_max_bytes', value: GB, type: 'int' },
    { key: 'git_sync_timeout_seconds', value: 600, type: 'int' },
    { key: 'git_sync_concurrency', value: 2, type: 'int' },
    { key: 'git_build_stall_seconds', value: 120, type: 'int' },
    { key: 'git_build_timeout_seconds', value: 1800, type: 'int' },
  ]
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
      bytes_served: 3 * GB,
      bytes_origin: GB,
      degraded: false,
      retry_at: 0,
    },
    {
      upstream_id: 2,
      host: 'deb.debian.org',
      requests: 200,
      hits: 100,
      denied: 0,
      origin_errors: 0,
      bytes_served: GB,
      bytes_origin: GB,
      degraded: false,
      retry_at: 0,
    },
  ],
}

const publicUpstreams = {
  list: [
    {
      host: 'docker.io',
      protocols: ['registry'],
      package_profile: 'none',
      library_completion: true,
      hit_rate: 0.942,
      cache_bytes: 812 * GB,
      status: 'normal',
    },
    {
      host: 'deb.debian.org',
      protocols: ['static'],
      package_profile: 'apt',
      package_readiness: pendingAPTReadiness,
      library_completion: false,
      hit_rate: 0.961,
      cache_bytes: 431 * GB,
      status: 'normal',
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
  origin_errors: 0,
  bytes_served: 4 * GB,
  bytes_origin: 2 * GB,
  cache_bytes: 1243 * GB,
  daily: [],
}

/**
 * 试算：只有 library/alpine:3.21 会被拒，其余落到默认策略。
 *
 * trace 是**求值顺序**（后端按具体度排序之后走过的那几步），止于决定结果的那一步，
 * 和规则表里的行序不是一回事。
 */
function ruleTest(host: string, path: string) {
  if (path.includes('alpine:3.21')) {
    return {
      host: host || 'docker.io',
      path,
      allowed: false,
      scope: 'upstream',
      matched_rule: ruleRows()[2],
      default_policy: 'allow_all',
      trace: [
        {
          scope: 'global',
          rule_id: 11,
          pattern: '*:latest',
          action: 'deny',
          matched: false,
          decisive: false,
        },
        {
          scope: 'upstream',
          rule_id: 1,
          pattern: 'library/*',
          action: 'allow',
          matched: false,
          decisive: false,
        },
        {
          scope: 'upstream',
          rule_id: 3,
          pattern: 'library/alpine:3.21',
          action: 'deny',
          matched: true,
          decisive: true,
        },
      ],
    }
  }
  return {
    host: host || 'docker.io',
    path,
    allowed: true,
    scope: 'default',
    matched_rule: null,
    default_policy: 'allow_all',
    trace: [
      {
        scope: 'global',
        rule_id: 11,
        pattern: '*:latest',
        action: 'deny',
        matched: false,
        decisive: false,
      },
      { scope: 'default', rule_id: 0, pattern: '', action: 'allow', matched: true, decisive: true },
    ],
  }
}

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, msg: 'success', data }) }
}

/** 后端的业务拒绝：4xx + 信封里的码，msg 是一句中文/英文串，界面一个字都不该贴。 */
function rejected(code: number, msg: string) {
  return { ok: false, status: 400, json: async () => ({ code, msg, data: null }) }
}

const unauthorized = {
  ok: false,
  status: 401,
  json: async () => ({ code: 401, msg: 'Unauthorized: admin key invalid', data: null }),
}

interface Backend {
  upstreams: ReturnType<typeof upstreamRows>
  rules: ReturnType<typeof ruleRows>
  objects: ReturnType<typeof cacheRows>
  settings: ReturnType<typeof settingRows>
}

let backend: Backend

function stubFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      const header = new Headers(init?.headers).get('Authorization')
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {}
      calls.push({ url, method, key: header, body })
      const [path, search] = url.split('?')
      const query = new URLSearchParams(search ?? '')

      if (path === '/api/v1/system/version') {
        return envelope({ version: '0.1.0', commit: 'abc1234' })
      }
      if (path === '/api/v1/site') {
        return envelope({
          name: 'katch',
          base_url: 'https://katch.dev',
          package_profiles: packageProfiles,
        })
      }
      if (header !== `Bearer ${acceptedKey}`) {
        return unauthorized
      }
      if (path === '/api/v1/admin/stats/upstreams/series') {
        return envelope({
          range: '24h',
          from: TO - 24 * HOUR,
          to: TO,
          bucket_seconds: HOUR,
          list: [],
        })
      }
      if (path === '/api/v1/admin/stats/upstreams') {
        return envelope(stats)
      }
      if (path === '/api/v1/admin/upstreams') {
        if (method === 'POST') {
          const id = 9
          backend.upstreams.push({
            ...(backend.upstreams[0] as unknown as Record<string, unknown>),
            ...body,
            id,
          } as unknown as (typeof backend.upstreams)[number])
          return envelope({ id })
        }
        if (query.has('preview_package_profile')) {
          const profile = query.get('preview_package_profile')
          return envelope({
            list: backend.upstreams,
            preview: {
              ready: false,
              missing: [],
              companions:
                profile === 'pypi'
                  ? [
                      {
                        host: 'files.pythonhosted.org',
                        transport: 'static',
                        package_profile: 'pypi',
                        ready: false,
                        reason: 'missing',
                      },
                    ]
                  : [],
              guidance: {
                clients: profile === 'pypi' ? ['pip', 'uv', 'poetry'] : [],
                configuration: [],
                constraints: ['trailing_slash'],
                runtime_verified: false,
              },
            },
          })
        }
        return envelope({ list: backend.upstreams })
      }
      // 改一条已存在的上游：id 在路径上，请求体是它接下来的全貌。
      if (path.startsWith('/api/v1/admin/upstreams/') && method === 'PUT') {
        const id = Number(path.slice('/api/v1/admin/upstreams/'.length))
        const existing = backend.upstreams.find((item) => item.id === id)
        if (!existing) {
          return rejected(10001, 'upstream not found')
        }
        Object.assign(existing, body, { id })
        return envelope({ id })
      }
      if (path === '/api/v1/admin/events') {
        return envelope({ list: [] })
      }
      if (path === '/api/v1/upstreams') {
        return envelope(publicUpstreams)
      }
      if (path === '/api/v1/stats/overview') {
        return envelope(overview)
      }
      if (path === '/api/v1/admin/rules') {
        if (holdRules && method === 'GET') {
          await holdRules
        }
        if (method === 'POST') {
          const id = Number(body.id) || 42
          const existing = backend.rules.find((rule) => rule.id === id)
          if (existing) {
            Object.assign(existing, body, { id })
          } else {
            backend.rules.push({
              id,
              upstream_id: Number(body.upstream_id) || 0,
              action: String(body.action),
              pattern: String(body.pattern),
              note: String(body.note ?? ''),
              createtime: TO,
              updatetime: TO,
            })
          }
          return envelope({ id })
        }
        return envelope({ list: backend.rules })
      }
      if (path.startsWith('/api/v1/admin/rules/') && method === 'DELETE') {
        const id = Number(path.split('/').pop())
        backend.rules = backend.rules.filter((rule) => rule.id !== id)
        return envelope({})
      }
      if (path === '/api/v1/admin/rules/test') {
        return envelope(ruleTest(String(body.host ?? ''), String(body.path ?? '')))
      }
      if (path === '/api/v1/admin/cache/objects') {
        const keyword = query.get('keyword') ?? ''
        const upstreamID = Number(query.get('upstream_id') ?? '0')
        const list = backend.objects.filter(
          (object) =>
            (keyword === '' || object.key.includes(keyword.replaceAll('*', ''))) &&
            (upstreamID === 0 || object.upstream_id === upstreamID)
        )
        return envelope({ list, total: list.length, page: 1, size: 20 })
      }
      if (path === '/api/v1/admin/cache/purge') {
        const id = Number(body.id ?? 0)
        const upstreamID = Number(body.upstream_id ?? 0)
        const doomed = backend.objects.filter(
          (object) => !object.pinned && (id ? object.id === id : object.upstream_id === upstreamID)
        )
        const skipped = backend.objects.filter(
          (object) => object.pinned && (id ? object.id === id : object.upstream_id === upstreamID)
        ).length
        backend.objects = backend.objects.filter((object) => !doomed.includes(object))
        return envelope({ removed: doomed.length, skipped })
      }
      if (path.endsWith('/pin') && method === 'POST') {
        const id = Number(path.split('/').at(-2))
        const object = backend.objects.find((item) => item.id === id)
        if (object) {
          object.pinned = Boolean(body.pinned)
        }
        return envelope({})
      }
      if (path === '/api/v1/admin/settings') {
        if (method === 'POST') {
          const settings = (body.settings ?? {}) as Record<string, unknown>
          for (const [key, value] of Object.entries(settings)) {
            const row = backend.settings.find((item) => item.key === key)
            if (!row) {
              return rejected(10007, `no such setting ${key}`)
            }
            if (key === 'cache_reclaim_percent' && Number(value) > 100) {
              return rejected(10008, `setting ${key} invalid: must be between 1 and 100`)
            }
            row.value = value as typeof row.value
          }
          return envelope({ list: backend.settings })
        }
        return envelope({ list: backend.settings })
      }
      if (path === '/api/v1/admin/settings/admin-key') {
        acceptedKey = String(body.new_key)
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
  acceptedKey = KEY
  holdRules = null
  backend = {
    upstreams: upstreamRows(),
    rules: ruleRows(),
    objects: cacheRows(),
    settings: settingRows(),
  }
  await i18n.changeLanguage('zh-CN')
  stubFetch()
})

describe('后台 · 访问规则', () => {
  it('粘一个资源地址：给出判定、命中第几条与完整匹配过程', async () => {
    renderAdmin('/admin/upstreams/1/rules')

    await userEvent.type(await screen.findByLabelText('资源地址'), 'library/alpine:3.21')
    await userEvent.click(screen.getByRole('button', { name: '试算' }))

    const verdict = await screen.findByRole('status', { name: '试算结果' })
    expect(verdict).toHaveTextContent('拒绝')
    expect(verdict).toHaveTextContent('403')
    // 命中的是求值过程里的第 3 步，说明取自那条规则自己的备注。
    expect(verdict).toHaveTextContent('命中第 3 条规则')
    expect(verdict).toHaveTextContent('存在 CVE-2025-3104，禁止拉取')

    const trace = screen.getByRole('list', { name: '匹配过程' })
    const steps = within(trace).getAllByRole('listitem')
    expect(steps).toHaveLength(3)
    expect(steps[0]).toHaveTextContent('*:latest')
    expect(steps[0]).toHaveTextContent('不匹配')
    expect(steps[0]).toHaveTextContent('全局')
    expect(steps[2]).toHaveTextContent('library/alpine:3.21')
    expect(steps[2]).toHaveTextContent('命中')
    expect(steps[2]).toHaveTextContent('拒绝')

    // 试算问的是这个上游，路径原样送过去。
    const call = calls.find((item) => item.url === '/api/v1/admin/rules/test')
    expect(call?.method).toBe('POST')
    expect(call?.body).toEqual({ host: 'docker.io', path: 'library/alpine:3.21' })
    // 后端的枚举与英文串一个都不许露出来。
    for (const raw of ['upstream', 'deny', 'decisive', 'allow_all']) {
      expect(within(verdict).queryByText(raw)).not.toBeInTheDocument()
    }
  })

  it('没有规则命中时说清楚落到了默认策略', async () => {
    renderAdmin('/admin/upstreams/1/rules')

    await userEvent.type(await screen.findByLabelText('资源地址'), 'library/redis:7')
    await userEvent.click(screen.getByRole('button', { name: '试算' }))

    const verdict = await screen.findByRole('status', { name: '试算结果' })
    expect(verdict).toHaveTextContent('放行')
    expect(verdict).toHaveTextContent('默认策略')
    expect(verdict).not.toHaveTextContent('命中第')
  })

  it('规则表只列这个上游的规则，全局规则另说', async () => {
    renderAdmin('/admin/upstreams/1/rules')

    const table = await screen.findByRole('table', { name: '访问规则' })
    expect(table).toHaveClass('table-fixed', 'min-w-[640px]')
    expect(table.parentElement).toHaveAttribute('data-slot', 'table-container')
    expect(table.parentElement).toHaveClass('min-w-0', 'overflow-x-auto')
    const pattern = await within(table).findByText('library/*')
    expect(pattern).toHaveClass('block', 'truncate')
    expect(pattern).toHaveAttribute('title', 'library/*')
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    expect(rows.map((row) => row.textContent).join(' ')).toContain('library/*')
    expect(rows.map((row) => row.textContent).join(' ')).not.toContain('*:latest')
    expect(rows[0]).toHaveTextContent('允许')
    // 全局规则先于本表求值，条数要说出来。
    expect(screen.getByText(/另有 1 条全局规则/)).toBeInTheDocument()
  })

  it('添加一条规则之后表里就有它', async () => {
    renderAdmin('/admin/upstreams/1/rules')

    await userEvent.click(await screen.findByRole('button', { name: '添加规则' }))
    await userEvent.type(screen.getByLabelText('路径模式'), 'bitnami/*')
    await userEvent.type(screen.getByLabelText('说明'), '平台组在用')
    await userEvent.click(screen.getByRole('button', { name: '保存规则' }))

    const table = await screen.findByRole('table', { name: '访问规则' })
    expect(await within(table).findByText('bitnami/*')).toBeInTheDocument()
    const call = calls.find((item) => item.url === '/api/v1/admin/rules' && item.method === 'POST')
    expect(call?.body).toMatchObject({
      upstream_id: 1,
      action: 'allow',
      pattern: 'bitnami/*',
      note: '平台组在用',
    })
  })

  it('默认策略改一次就写回后端', async () => {
    renderAdmin('/admin/upstreams/1/rules')

    await userEvent.click(await screen.findByRole('radio', { name: '仅允许命中规则' }))

    const call = calls.find(
      (item) => item.url === '/api/v1/admin/upstreams/1' && item.method === 'PUT'
    )
    expect(call?.body).toMatchObject({ default_policy: 'deny_unless_matched' })
  })
})

describe('后台 · 缓存对象', () => {
  it('搜到一个对象并清除它', async () => {
    renderAdmin('/admin/cache')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    expect(table).toHaveClass('table-fixed', 'min-w-[860px]')
    expect(table.parentElement).toHaveAttribute('data-slot', 'table-container')
    expect(table.parentElement).toHaveClass('min-w-0', 'overflow-x-auto')
    expect(within(table).getAllByRole('row').slice(1)).toHaveLength(2)

    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'redis')

    const objectKey = await within(table).findByText('library/redis:7')
    expect(objectKey).toHaveClass('block', 'truncate')
    expect(objectKey).toHaveAttribute('title', 'library/redis:7')
    await vi.waitFor(() => {
      expect(within(table).queryByText(/libc6/)).not.toBeInTheDocument()
    })
    expect(
      calls.some((call) => call.url.includes('/admin/cache/objects?') && call.url.includes('redis'))
    ).toBe(true)

    const row = within(table).getByRole('row', { name: /library\/redis:7/ })
    await userEvent.click(within(row).getByRole('button', { name: '清除' }))

    await vi.waitFor(() => {
      expect(within(table).queryByText('library/redis:7')).not.toBeInTheDocument()
    })
    const purge = calls.find((call) => call.url === '/api/v1/admin/cache/purge')
    expect(purge?.body).toMatchObject({ id: 101 })
  })

  it('容量条按上游分段，配额与水位来自设置', async () => {
    renderAdmin('/admin/cache')

    const bar = await screen.findByRole('img', { name: '缓存容量' })
    const segments = bar.querySelectorAll('[data-slot="capacity-segment"]')
    expect(segments).toHaveLength(2)
    // 812 GB + 431 GB = 1.21 TB，配额 3 TB（字节数一律按 formatBytes 的两位小数写）。
    expect(await screen.findByText('1.21')).toBeInTheDocument()
    expect(await screen.findByText(/配额 3\.00 TB/)).toBeInTheDocument()
    expect(screen.getByText(/85%/)).toBeInTheDocument()
  })

  it('选中的全是已固定的对象时，那枚按钮换成放开', async () => {
    renderAdmin('/admin/cache')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const row = within(table).getByRole('row', { name: /libc6/ })
    await userEvent.click(within(row).getByRole('checkbox'))

    await userEvent.click(await screen.findByRole('button', { name: '取消固定' }))

    const pin = calls.find((call) => call.url.endsWith('/pin'))
    expect(pin?.url).toContain('/admin/cache/objects/102/pin')
    expect(pin?.body).toEqual({ pinned: false })
  })

  it('固定过的对象在批量清除里被跳过，界面把跳过的条数说出来', async () => {
    renderAdmin('/admin/upstreams/2')

    await userEvent.click(await screen.findByRole('button', { name: '清空缓存' }))
    await userEvent.click(await screen.findByRole('button', { name: '确认清空' }))

    expect(await screen.findByRole('status')).toHaveTextContent('1 个已固定的对象保留')
    const purge = calls.find((call) => call.url === '/api/v1/admin/cache/purge')
    expect(purge?.body).toMatchObject({ upstream_id: 2 })
  })
})

describe('后台 · 设置', () => {
  it('保存一个运行时值，并写明哪些设置在 configs/config.yaml 里', async () => {
    renderAdmin('/admin/settings')

    expect(await screen.findByText(/configs\/config\.yaml/)).toBeInTheDocument()
    expect(screen.getByText(/监听地址、数据库、日志/)).toBeInTheDocument()
    // 运行时设置改完立刻生效，这一面不许再说「需要重启」。
    expect(screen.getByRole('heading', { name: '设置' })).toBeInTheDocument()

    const concurrency = await screen.findByLabelText('回源并发上限')
    await userEvent.clear(concurrency)
    await userEvent.type(concurrency, '128')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/settings' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toEqual({ settings: { origin_concurrency: 128 } })
    expect(await screen.findByRole('status')).toHaveTextContent('已保存')
  })

  it('git 镜像那六项读得出来，改一项存得回去', async () => {
    // 设置页不按后端的 settingDefs 自动渲染，每一项都是手写的一行：少写一行，
    // 那一项就是「存得下但界面上根本改不到」。
    renderAdmin('/admin/settings')

    expect(await screen.findByLabelText('镜像总配额')).toHaveValue('10 GB')
    expect(screen.getByLabelText('同步超时')).toHaveValue('10m')
    expect(screen.getByLabelText('同步并发上限')).toHaveValue('2')
    expect(screen.getByLabelText('建镜像停滞时限')).toHaveValue('2m')
    expect(screen.getByLabelText('建镜像总时限')).toHaveValue('30m')

    const limit = await screen.findByLabelText('单仓体积上限')
    expect(limit).toHaveValue('1.00 GB')
    await userEvent.clear(limit)
    await userEvent.type(limit, '512 MB')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/settings' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toEqual({ settings: { git_repo_max_bytes: 512 * 1024 * 1024 } })
    expect(await screen.findByRole('status')).toHaveTextContent('已保存')
  })

  it('两道时间闸改完按数送回后端，不是把 `5m` 原样送过去', async () => {
    renderAdmin('/admin/settings')

    const stall = await screen.findByLabelText('建镜像停滞时限')
    await userEvent.clear(stall)
    await userEvent.type(stall, '30s')
    const total = screen.getByLabelText('建镜像总时限')
    await userEvent.clear(total)
    await userEvent.type(total, '1h')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/settings' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toEqual({
      settings: { git_build_stall_seconds: 30, git_build_timeout_seconds: 3600 },
    })
    expect(await screen.findByRole('status')).toHaveTextContent('已保存')
  })

  it('最近请求保留时长默认一天，写成 1h 就按秒保存', async () => {
    renderAdmin('/admin/settings')

    const retention = await screen.findByLabelText('最近请求保留时长')
    expect(retention).toHaveValue('24h')

    await userEvent.clear(retention)
    await userEvent.type(retention, '1h')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/settings' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toEqual({ settings: { recent_request_retention_seconds: 3600 } })
  })

  it('保留期写成 3d 也认，按三天保存', async () => {
    renderAdmin('/admin/settings')

    const retention = await screen.findByLabelText('最近请求保留时长')
    await userEvent.clear(retention)
    await userEvent.type(retention, '3d')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/settings' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toEqual({ settings: { recent_request_retention_seconds: 259200 } })
  })

  it('保留期的写法看不懂时拦住保存，一个字也不发给后端', async () => {
    renderAdmin('/admin/settings')

    const retention = await screen.findByLabelText('最近请求保留时长')
    await userEvent.clear(retention)
    await userEvent.type(retention, '两天')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('有一个值看不懂')
    expect(
      calls.some((item) => item.url === '/api/v1/admin/settings' && item.method === 'POST')
    ).toBe(false)
  })

  it('后端拒绝一个值时说的是翻译过的话，不是后端的串', async () => {
    renderAdmin('/admin/settings')

    const watermark = await screen.findByLabelText('回收水位')
    await userEvent.clear(watermark)
    await userEvent.type(watermark, '140')
    await userEvent.click(screen.getByRole('button', { name: '保存更改' }))

    const failure = await screen.findByRole('alert')
    expect(failure).toHaveTextContent('取值不合法')
    expect(screen.queryByText(/must be between/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/10008/)).not.toBeInTheDocument()
  })

  it('轮换时还在路上的那个请求 401 回来，不算会话失效', async () => {
    // 轮换的那一刻，用旧密钥发出去的请求还在路上；它回来时必然是 401。把那个
    // 401 当成「密钥失效」会把刚换完密钥的人当场踢回登录页。
    let answer: ((value: unknown) => void) | null = null
    const inflight = new Promise((resolve) => {
      answer = resolve
    })
    holdRules = inflight
    renderAdmin('/admin/settings')

    await userEvent.click(await screen.findByRole('button', { name: '轮换' }))
    await userEvent.type(screen.getByLabelText('新的管理密钥'), 'a-brand-new-admin-key')
    await userEvent.click(screen.getByRole('button', { name: '确认轮换' }))
    await vi.waitFor(() => {
      expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBe('a-brand-new-admin-key')
    })

    // 现在才让那个用旧密钥发出去的请求回来。
    answer!(null)
    await inflight

    expect(await screen.findByRole('heading', { name: '设置' })).toBeInTheDocument()
    expect(screen.queryByLabelText('管理员密钥')).not.toBeInTheDocument()
    expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBe('a-brand-new-admin-key')
  })

  it('轮换密钥之后浏览器手上的那把跟着换，不把人晾在一个坏掉的会话里', async () => {
    renderAdmin('/admin/settings')

    await userEvent.click(await screen.findByRole('button', { name: '轮换' }))
    await userEvent.type(screen.getByLabelText('新的管理密钥'), 'a-brand-new-admin-key')
    await userEvent.click(screen.getByRole('button', { name: '确认轮换' }))

    await vi.waitFor(() => {
      expect(localStorage.getItem(ADMIN_KEY_STORAGE)).toBe('a-brand-new-admin-key')
    })
    // 旧密钥已经不被接受了，但界面还在设置页上，不该被踢回登录。
    expect(await screen.findByRole('heading', { name: '设置' })).toBeInTheDocument()
    expect(screen.queryByLabelText('管理员密钥')).not.toBeInTheDocument()
  })
})

describe('后台 · 上游的增改与启停', () => {
  it('添加一个上游之后侧栏能进到它', async () => {
    renderAdmin('/admin')

    await userEvent.click(await screen.findByRole('link', { name: '添加上游' }))
    await userEvent.type(screen.getByLabelText('上游主机名'), 'ghcr.io')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://ghcr.io')
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toMatchObject({
      host: 'ghcr.io',
      origin: 'https://ghcr.io',
      protocols: ['registry'],
      enabled: true,
    })
  })

  it('协议是多选：勾上 git 不会把原本的静态资源挤掉', async () => {
    renderAdmin('/admin')

    await userEvent.click(await screen.findByRole('link', { name: '添加上游' }))
    await userEvent.type(screen.getByLabelText('上游主机名'), 'github.com')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://github.com')
    // 出厂勾的是容器镜像，这一条上游要的是静态资源加 git。
    await userEvent.click(screen.getByRole('checkbox', { name: '容器镜像' }))
    // 故意倒着勾：写回去的顺序是固定的那一份，不随点击先后变——否则同一条上游
    // 会有两份说法相同、字节不同的请求体。
    await userEvent.click(screen.getByRole('checkbox', { name: 'Git 仓库' }))
    await userEvent.click(screen.getByRole('checkbox', { name: '静态资源' }))
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.protocols).toEqual(['static', 'git'])
  })

  it('一个协议都不勾时不发请求：那条记录什么都服务不了', async () => {
    renderAdmin('/admin')

    await userEvent.click(await screen.findByRole('link', { name: '添加上游' }))
    await userEvent.type(screen.getByLabelText('上游主机名'), 'ghcr.io')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://ghcr.io')
    await userEvent.click(screen.getByRole('checkbox', { name: '容器镜像' }))
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    expect(
      calls.some((item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST')
    ).toBe(false)
  })

  it('暂停一个上游写的是 enabled=false，其余登记字段原样带回去', async () => {
    renderAdmin('/admin/upstreams/1')

    await userEvent.click(await screen.findByRole('button', { name: '暂停' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/1' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    // enabled 必须真的是 false 而不是缺省：两者在 JSON 里长得一样，
    // 少了这个字段，后端看到的就是一次「没提启停」的改动。
    expect(call.body.enabled).toBe(false)
    expect(call.body).toMatchObject({
      host: 'docker.io',
      origin: 'https://registry-1.docker.io',
      library_completion: true,
      mutable_ttl_seconds: 300,
    })
  })

  it('暂停之后按钮变成恢复，再按一次写的是 enabled=true', async () => {
    renderAdmin('/admin/upstreams/1')

    await userEvent.click(await screen.findByRole('button', { name: '暂停' }))
    await userEvent.click(await screen.findByRole('button', { name: '恢复' }))

    const resumed = await vi.waitFor(() => {
      const found = calls.filter(
        (item) => item.url === '/api/v1/admin/upstreams/1' && item.method === 'PUT'
      )
      expect(found).toHaveLength(2)
      return found[1]!
    })
    expect(resumed.body.enabled).toBe(true)
  })

  it('编辑表单保存时改的是这一条，而不是再登记一条新的', async () => {
    renderAdmin('/admin/upstreams/1/edit')

    const origin = await screen.findByLabelText('回源地址')
    await userEvent.clear(origin)
    await userEvent.type(origin, 'https://mirror.example.com')
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/1' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toMatchObject({ host: 'docker.io', origin: 'https://mirror.example.com' })
    expect(
      calls.some((item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST')
    ).toBe(false)
  })

  it('这条上游已经被别人删掉时，暂停按钮说的是它不在了，而不是一句泛泛的失败', async () => {
    renderAdmin('/admin/upstreams/1')

    // 界面手上这份列表是刚才拉的，而这条上游此刻已经不在后端了。
    await screen.findByRole('button', { name: '暂停' })
    backend.upstreams = backend.upstreams.filter((item) => item.id !== 1)

    await userEvent.click(screen.getByRole('button', { name: '暂停' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('这条上游已经不在了')
  })

  it('编辑一个上游时表单里是它自己的登记信息', async () => {
    renderAdmin('/admin/upstreams/1')

    await userEvent.click(await screen.findByRole('button', { name: '编辑' }))

    expect(await screen.findByLabelText('上游主机名')).toHaveValue('docker.io')
    expect(screen.getByLabelText('回源地址')).toHaveValue('https://registry-1.docker.io')
  })

  it('选一个预设只填普通模式，保存的还是模式本身', async () => {
    renderAdmin('/admin')

    await userEvent.click(await screen.findByRole('link', { name: '添加上游' }))
    await userEvent.type(screen.getByLabelText('上游主机名'), 'proxy.golang.org')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://proxy.golang.org')

    const policy = screen.getByRole('radiogroup', { name: '缓存策略' })
    expect(
      within(policy)
        .getAllByRole('radio')
        .map((item) => item.textContent)
    ).toEqual(['自定义', 'APT 软件包', 'Go Module Proxy', 'Git commit 静态文件', 'PyPI 文件'])
    expect(within(policy).getByRole('radio', { name: '自定义' })).toHaveAttribute(
      'aria-checked',
      'true'
    )
    await userEvent.click(within(policy).getByRole('radio', { name: 'Go Module Proxy' }))

    expect(screen.getByLabelText('不可变路径模式')).toHaveValue('/@v/*.info\n/@v/*.mod\n/@v/*.zip')
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.immutable_patterns).toEqual(['/@v/*.info', '/@v/*.mod', '/@v/*.zip'])
    // 预设名称不进请求体：上游记录始终是唯一真相。
    expect(call.body).not.toHaveProperty('preset')
  })

  it('预设填进去之后照常逐行编辑', async () => {
    renderAdmin('/admin/upstreams/2/edit')

    const policy = await screen.findByRole('radiogroup', { name: '缓存策略' })
    await userEvent.click(within(policy).getByRole('radio', { name: 'APT 软件包' }))
    fireEvent.change(screen.getByLabelText('不可变路径模式'), {
      target: { value: '/pool/\n/pool/main/' },
    })
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/2' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.immutable_patterns).toEqual(['/pool/', '/pool/main/'])
  })

  it('模式与某个预设逐项相同时显示那个预设', async () => {
    backend.upstreams[1]!.immutable_patterns = ['/@v/*.info', '/@v/*.mod', '/@v/*.zip']
    renderAdmin('/admin/upstreams/2/edit')

    const policy = await screen.findByRole('radiogroup', { name: '缓存策略' })
    expect(within(policy).getByRole('radio', { name: 'Go Module Proxy' })).toHaveAttribute(
      'aria-checked',
      'true'
    )
  })

  it('模式内容相同但顺序不同时显示自定义，不替使用者猜是哪个预设', async () => {
    backend.upstreams[1]!.immutable_patterns = ['/@v/*.mod', '/@v/*.info', '/@v/*.zip']
    renderAdmin('/admin/upstreams/2/edit')

    const policy = await screen.findByRole('radiogroup', { name: '缓存策略' })
    expect(within(policy).getByRole('radio', { name: '自定义' })).toHaveAttribute(
      'aria-checked',
      'true'
    )
  })

  it('切回自定义不会清掉已经填好的模式', async () => {
    renderAdmin('/admin/upstreams/2/edit')

    const pypi = '/packages/??/??/' + '?'.repeat(64) + '/'
    const policy = await screen.findByRole('radiogroup', { name: '缓存策略' })
    await userEvent.click(within(policy).getByRole('radio', { name: 'PyPI 文件' }))
    await userEvent.click(within(policy).getByRole('radio', { name: '自定义' }))

    expect(screen.getByLabelText('不可变路径模式')).toHaveValue(pypi)
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))
    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/2' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.immutable_patterns).toEqual([pypi])
  })

  it('保存前逐行去掉空白与空行，重复项只留第一次出现的位置', async () => {
    renderAdmin('/admin/upstreams/2/edit')

    const patterns = await screen.findByLabelText('不可变路径模式')
    fireEvent.change(patterns, {
      target: { value: '  /pool/  \n\n/pool/\ndists/\n  dists/  \n' },
    })
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/2' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.immutable_patterns).toEqual(['/pool/', 'dists/'])
  })

  it('可变对象 TTL 是非负整数，0 表示沿用全局默认', async () => {
    renderAdmin('/admin/upstreams/2/edit')

    const ttl = await screen.findByLabelText('可变对象 TTL（秒）')
    expect(ttl).toHaveValue('600')
    await userEvent.clear(ttl)
    await userEvent.type(ttl, '0')
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams/2' && item.method === 'PUT'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body.mutable_ttl_seconds).toBe(0)
  })

  it('TTL 写成负数时不发请求', async () => {
    renderAdmin('/admin/upstreams/2/edit')

    const ttl = await screen.findByLabelText('可变对象 TTL（秒）')
    await userEvent.clear(ttl)
    await userEvent.type(ttl, '-5')
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('TTL')
    expect(calls.some((item) => item.method === 'PUT')).toBe(false)
  })

  it('不因为主机名叫 deb.debian.org 就替使用者套上 APT 预设', async () => {
    renderAdmin('/admin')

    await userEvent.click(await screen.findByRole('link', { name: '添加上游' }))
    await userEvent.type(screen.getByLabelText('上游主机名'), 'deb.debian.org')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://deb.debian.org')

    const policy = screen.getByRole('radiogroup', { name: '缓存策略' })
    expect(within(policy).getByRole('radio', { name: '自定义' })).toHaveAttribute(
      'aria-checked',
      'true'
    )
    expect(screen.getByLabelText('不可变路径模式')).toHaveValue('')
  })

  it('profile 选项来自后端，并把草稿 readiness 与 profile 一起保存', async () => {
    renderAdmin('/admin/upstreams/new')

    await userEvent.type(await screen.findByLabelText('上游主机名'), 'pypi.org')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://pypi.org')
    await userEvent.click(screen.getByRole('checkbox', { name: '容器镜像' }))
    await userEvent.click(screen.getByRole('checkbox', { name: '静态资源' }))

    const profile = await screen.findByRole('combobox', { name: '包管理器配置' })
    expect(
      within(profile)
        .getAllByRole('option')
        .map((option) => option.textContent)
    ).toEqual(['无', 'APT', 'PyPI'])
    await userEvent.selectOptions(profile, 'pypi')

    const readiness = await screen.findByRole('region', { name: '包管理器就绪状态' })
    expect(readiness).toHaveTextContent('配置不完整')
    expect(readiness).toHaveTextContent('files.pythonhosted.org')
    expect(readiness).toHaveTextContent('缺少上游')
    expect(readiness).toHaveTextContent('尚未通过真实客户端验证')

    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))
    const call = await vi.waitFor(() => {
      const found = calls.find(
        (item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST'
      )
      expect(found).toBeDefined()
      return found!
    })
    expect(call.body).toMatchObject({ package_profile: 'pypi', protocols: ['static'] })
  })

  it('非 none profile 没有 static transport 时在界面拒绝保存', async () => {
    renderAdmin('/admin/upstreams/new')

    await userEvent.type(await screen.findByLabelText('上游主机名'), 'pypi.org')
    await userEvent.type(screen.getByLabelText('回源地址'), 'https://pypi.org')
    await userEvent.selectOptions(
      await screen.findByRole('combobox', { name: '包管理器配置' }),
      'pypi'
    )
    await userEvent.click(screen.getByRole('button', { name: '保存上游' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('静态资源')
    expect(
      calls.some((item) => item.url === '/api/v1/admin/upstreams' && item.method === 'POST')
    ).toBe(false)
  })

  it('管理详情展示后端 readiness，但 pending 时不展示可复制配置', async () => {
    renderAdmin('/admin/upstreams/2')

    const readiness = await screen.findByRole('region', { name: '包管理器就绪状态' })
    expect(readiness).toHaveTextContent('配置已就绪')
    expect(readiness).toHaveTextContent('尚未通过真实客户端验证')
    expect(screen.queryByText(/deb \[signed-by=/)).not.toBeInTheDocument()
  })
})
