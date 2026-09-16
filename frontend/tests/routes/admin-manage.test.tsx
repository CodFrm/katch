import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom'
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

function upstreamRows() {
  return [
    {
      id: 1,
      host: 'docker.io',
      protocols: ['registry'],
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

/** 缓存键的变体段分隔符，同后端 variantMarker。 */
const VARIANT = '\u001f'

function cacheObject(
  id: number,
  upstreamID: number,
  key: string,
  extra: Record<string, unknown> = {}
) {
  return {
    id,
    upstream_id: upstreamID,
    key,
    digest: `sha256:${String(id).repeat(4)}`,
    size: 1024 * id,
    immutable: true,
    pinned: false,
    expires_at: 0,
    last_access_at: TO - id,
    hit_count: id,
    createtime: TO - 90000,
    updatetime: TO - id,
    ...extra,
  }
}

function cacheRows() {
  return [
    cacheObject(101, 1, '/library/redis/manifests/7', {
      digest: 'sha256:9f3a2c',
      size: 432013312,
      last_access_at: TO - 120,
      hit_count: 12480,
    }),
    cacheObject(
      106,
      1,
      `/library/redis/manifests/7${VARIANT}application/vnd.oci.image.index.v1+json`
    ),
    cacheObject(
      107,
      1,
      '/library/redis/blobs/sha256:4f1e0c1a5d0b7d2c6e8a9b3f2d1c0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d'
    ),
    cacheObject(102, 2, '/pool/main/g/glibc/libc6_2.41-1_amd64.deb', {
      digest: 'sha256:4b1e77',
      size: 2936012,
      pinned: true,
      last_access_at: TO - 60,
      hit_count: 41203,
    }),
    cacheObject(104, 2, '/pool/main/r/redis/redis-server_7.0.15_amd64.deb'),
    cacheObject(105, 2, '/dists/bookworm/InRelease?by-hash=/x/y'),
  ]
}

/** 一条键拆成目录段、叶子名（查询串留在叶子上）与是否变体，同后端 splitTreeKey。 */
function splitKey(key: string) {
  const marker = key.indexOf(VARIANT)
  const plain = marker >= 0 ? key.slice(0, marker) : key
  const question = plain.indexOf('?')
  const pathPart = question >= 0 ? plain.slice(0, question) : plain
  const query = question >= 0 ? plain.slice(question) : ''
  const parts = pathPart.slice(1).split('/')
  const leaf = `${parts.pop() ?? ''}${query}`
  return { dirs: parts, leaf, variant: marker >= 0 }
}

type CacheRow = ReturnType<typeof cacheRows>[number]

/** 一个目录路径下（递归）的全部对象，带着各自的目录段与叶子名。 */
function objectsUnder(path: string) {
  const segments = path === '' ? [] : path.split('/')
  return backend.objects.flatMap((object) => {
    const host = backend.upstreams.find((item) => item.id === object.upstream_id)?.host ?? ''
    const { dirs, leaf, variant } = splitKey(object.key)
    const full = [host, ...dirs]
    if (segments.some((segment, index) => full[index] !== segment)) {
      return []
    }
    if (segments.length > full.length) {
      return []
    }
    return [{ object, host, full, leaf, variant, path: [...full, leaf].join('/') }]
  })
}

function treeStat(rows: { object: CacheRow }[]) {
  return {
    count: rows.length,
    pinned_count: rows.filter((row) => row.object.pinned).length,
    size: rows.reduce((sum, row) => sum + row.object.size, 0),
    last_access_at: Math.max(0, ...rows.map((row) => row.object.last_access_at)),
  }
}

function cacheTree(path: string, offset: number) {
  const segments = path === '' ? [] : path.split('/')
  const rows = objectsUnder(path)
  const dirNames = [
    ...new Set(
      rows
        .filter((row) => row.full.length > segments.length)
        .map((row) => row.full[segments.length])
    ),
  ].sort()
  const dirs = dirNames.map((name) => {
    const child = [...segments, name].join('/')
    return { kind: 'dir', name, path: child, variant: false, ...treeStat(objectsUnder(child)) }
  })
  const objects = rows
    .filter((row) => row.full.length === segments.length)
    .sort((a, b) => a.leaf.localeCompare(b.leaf))
    .map((row) => ({
      kind: 'object',
      name: row.leaf,
      path: row.path,
      variant: row.variant,
      count: 0,
      pinned_count: 0,
      size: 0,
      last_access_at: 0,
      object: { ...row.object, host: row.host },
    }))
  const all = [...dirs, ...objects]
  const page = all.slice(offset, offset + backend.treeLimit)
  return {
    path,
    total_count: rows.length,
    total_pinned: rows.filter((row) => row.object.pinned).length,
    total_size: rows.reduce((sum, row) => sum + row.object.size, 0),
    children: page,
    has_more: offset + page.length < all.length,
    next_offset: offset + page.length,
  }
}

function cacheTreeSearch(path: string, keyword: string) {
  const needle = keyword.toLowerCase()
  const base = path === '' ? 0 : path.split('/').length
  const matchedRows = objectsUnder(path).filter((row) =>
    row.path.split('/').slice(base).join('/').toLowerCase().includes(needle)
  )
  const shown = matchedRows.slice(0, backend.searchLimit)
  const dirPaths = new Set<string>()
  for (const row of shown) {
    for (let depth = base + 1; depth <= row.full.length; depth++) {
      dirPaths.add(row.full.slice(0, depth).join('/'))
    }
  }
  return {
    path,
    matched: matchedRows.length,
    truncated: matchedRows.length > shown.length,
    objects: shown.map((row) => ({
      name: row.leaf,
      path: row.path,
      variant: row.variant,
      object: { ...row.object, host: row.host },
    })),
    dirs: [...dirPaths].sort().map((dir) => {
      const all = treeStat(objectsUnder(dir))
      const matched = treeStat(matchedRows.filter((row) => row.path.startsWith(`${dir}/`)))
      return {
        path: dir,
        name_match: (dir.split('/').at(-1) ?? '').toLowerCase().includes(needle),
        count: all.count,
        size: all.size,
        matched_count: matched.count,
        matched_size: matched.size,
        last_access_at: all.last_access_at,
      }
    }),
  }
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
      library_completion: true,
      hit_rate: 0.942,
      cache_bytes: 812 * GB,
      status: 'normal',
    },
    {
      host: 'deb.debian.org',
      protocols: ['static'],
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

/** 后端内部出错（比如 SQL 执行失败）时的回应：500 加上泛泛的「系统错误」。 */
function systemError() {
  return { ok: false, status: 500, json: async () => ({ code: -1, msg: '系统错误', data: null }) }
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
  /** 目录树每层一批给几项，真实后端是 200；调小了才测得到「加载更多」。 */
  treeLimit: number
  /** 目录搜索最多给几个对象，真实后端是 200。 */
  searchLimit: number
  /** 让按目录清除被后端按参数错误拒绝。 */
  failTreePurge: boolean
  /** 让根以下的目录层读取出内部错误（根那一层照常给）。 */
  failTree: boolean
  /** 让目录搜索出内部错误。 */
  failTreeSearch: boolean
  /** 让根以下的目录层读取出内部错误，但只在搜索开始之后（搜索结果里按目录清除要单独问合计）。 */
  failTreeAfterSearch: boolean
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
      if (path === '/api/v1/admin/cache/tree') {
        if (
          (backend.failTree ||
            (backend.failTreeAfterSearch &&
              calls.some((item) => item.url.startsWith('/api/v1/admin/cache/tree/search')))) &&
          (query.get('path') ?? '') !== ''
        ) {
          return systemError()
        }
        return envelope(cacheTree(query.get('path') ?? '', Number(query.get('offset') ?? '0')))
      }
      if (path === '/api/v1/admin/cache/tree/search') {
        if (backend.failTreeSearch) {
          return systemError()
        }
        return envelope(cacheTreeSearch(query.get('path') ?? '', query.get('keyword') ?? ''))
      }
      if (path === '/api/v1/admin/cache/tree/purge') {
        if (backend.failTreePurge) {
          return rejected(10010, 'cache tree path invalid')
        }
        const under = objectsUnder(String(body.path ?? '')).map((row) => row.object)
        const doomed = under.filter((object) => !object.pinned)
        backend.objects = backend.objects.filter((object) => !doomed.includes(object))
        return envelope({ removed: doomed.length, skipped: under.length - doomed.length })
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
    treeLimit: 200,
    searchLimit: 200,
    failTreePurge: false,
    failTree: false,
    failTreeSearch: false,
    failTreeAfterSearch: false,
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

/** 把当前地址的查询串挂在一个节点上，断言 ?path= 用。 */
function LocationProbe() {
  const location = useLocation()
  return <span data-testid="location" data-search={location.search} />
}

/** 浏览器的后退键：MemoryRouter 不走 window.history，只能从路由里后退。 */
function BackProbe() {
  const navigate = useNavigate()
  return <button type="button" data-testid="back" onClick={() => navigate(-1)} />
}

function renderCache(path = '/admin/cache') {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <LocationProbe />
      <BackProbe />
    </MemoryRouter>
  )
}

/**
 * 把命中 match 的请求扣在半路，release 之后才放行；settled 是已经放行完的个数。
 * 用来摆出「同一批被连点两次」「旧响应比新响应后到」这类只有时序才碰得到的场面。
 */
function holdFetch(match: (url: string) => boolean, limit = Infinity) {
  const inner = globalThis.fetch
  let release = () => {}
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  const state = { held: 0, settled: 0, release: () => release() }
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      if (!match(url) || state.held >= limit) {
        return inner(url, init)
      }
      state.held += 1
      // 先问后端再扣：扣住的是一份按发出那一刻的库算出来的响应。
      const response = await inner(url, init)
      await gate
      state.settled += 1
      return response
    })
  )
  return state
}

/** 让已经放行的响应把状态更新落完：json() 与 setState 各自还隔着几轮微任务。 */
async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20))
  })
}

function treeRow(table: HTMLElement, name: RegExp) {
  return within(table).getByRole('row', { name })
}

describe('后台 · 缓存对象', () => {
  it('根上列出有缓存的上游主机，点三角就地展开下一层，再点收起', async () => {
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    expect(table).toHaveClass('table-fixed', 'min-w-[860px]')
    expect(table.parentElement).toHaveAttribute('data-slot', 'table-container')
    expect(table.parentElement).toHaveClass('min-w-0', 'overflow-x-auto')
    await within(table).findByText('docker.io/')
    expect(within(table).getAllByRole('row').slice(1)).toHaveLength(2)
    expect(calls.some((call) => call.url === '/api/v1/admin/cache/tree?path=')).toBe(true)

    const deb = treeRow(table, /deb\.debian\.org\//)
    // 目录行：对象数是递归合计，命中是「—」，不能勾选。
    expect(within(deb).getByText('3')).toBeInTheDocument()
    expect(within(deb).queryByRole('checkbox')).not.toBeInTheDocument()

    await userEvent.click(within(deb).getByRole('button', { name: '展开 deb.debian.org' }))
    expect(await within(table).findByText('dists/')).toBeInTheDocument()
    expect(within(table).getByText('pool/')).toBeInTheDocument()
    expect(screen.getByTestId('location')).toHaveAttribute('data-search', '')

    await userEvent.click(within(deb).getByRole('button', { name: '收起 deb.debian.org' }))
    await vi.waitFor(() => {
      expect(within(table).queryByText('pool/')).not.toBeInTheDocument()
    })
  })

  it('点目录名进入那一层，地址跟着变，后退回到上一层', async () => {
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await userEvent.click(await within(table).findByRole('button', { name: 'docker.io/' }))

    await vi.waitFor(() => {
      expect(screen.getByTestId('location')).toHaveAttribute('data-search', '?path=docker.io')
    })
    await userEvent.click(await within(table).findByRole('button', { name: 'library/' }))
    await userEvent.click(await within(table).findByRole('button', { name: '进入 redis' }))
    await vi.waitFor(() => {
      expect(screen.getByTestId('location')).toHaveAttribute(
        'data-search',
        `?${new URLSearchParams({ path: 'docker.io/library/redis' })}`
      )
    })

    const crumbs = screen.getByRole('navigation', { name: '目录路径' })
    expect(within(crumbs).getByRole('button', { name: '全部上游' })).toBeInTheDocument()
    expect(within(crumbs).getByRole('button', { name: 'docker.io' })).toBeInTheDocument()
    expect(within(crumbs).getByRole('button', { name: 'library' })).toBeInTheDocument()
    // 当前那一段不能点。
    expect(within(crumbs).queryByRole('button', { name: 'redis' })).not.toBeInTheDocument()
    expect(within(crumbs).getByText('redis')).toBeInTheDocument()
    expect(await within(table).findByText('manifests/')).toBeInTheDocument()

    await userEvent.click(screen.getByTestId('back'))
    expect(await within(table).findByText('redis/')).toBeInTheDocument()
    expect(screen.getByTestId('location')).toHaveAttribute(
      'data-search',
      `?${new URLSearchParams({ path: 'docker.io/library' })}`
    )

    await userEvent.click(within(crumbs).getByRole('button', { name: '全部上游' }))
    expect(await within(table).findByText('deb.debian.org/')).toBeInTheDocument()
  })

  it('对象行显示叶子名、短摘要、变体与固定标记，长名字截断且悬停看得到完整键', async () => {
    renderCache('/admin/cache?path=docker.io/library/redis/manifests')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const rows = await within(table).findAllByRole('row', { name: /^选择.*7/ })
    expect(rows).toHaveLength(2)
    expect(within(rows[1]).getByText('变体')).toBeInTheDocument()
    expect(within(rows[0]).queryByText('变体')).not.toBeInTheDocument()
    expect(within(rows[0]).getByText('@9f3a2c')).toBeInTheDocument()
    expect(within(rows[0]).getByText('—')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'redis' }))
    await userEvent.click(await within(table).findByRole('button', { name: 'blobs/' }))
    const leaf = await within(table).findByText(/^sha256:4f1e0c/)
    expect(leaf).toHaveClass('truncate')
    expect(leaf).toHaveAttribute(
      'title',
      'docker.io/library/redis/blobs/sha256:4f1e0c1a5d0b7d2c6e8a9b3f2d1c0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d'
    )
  })

  it('一层超过一批时末尾给「加载更多」，点了把下一批接在后面', async () => {
    backend.treeLimit = 2
    backend.objects.push(cacheObject(108, 2, '/pool/main/z.deb'))
    renderCache('/admin/cache?path=deb.debian.org/pool/main')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('g/')
    // 第一批只有 g/ 与 r/，那个对象要到下一批才出现。
    expect(within(table).queryByText('z.deb')).not.toBeInTheDocument()

    await userEvent.click(within(table).getByRole('button', { name: '加载更多' }))

    expect(await within(table).findByText('z.deb')).toBeInTheDocument()
    expect(within(table).getByText('g/')).toBeInTheDocument()
    expect(calls.some((call) => call.url.includes('offset=2'))).toBe(true)
    expect(within(table).queryByRole('button', { name: '加载更多' })).not.toBeInTheDocument()
  })

  it('「加载更多」在下一批回来之前连点两下，下一批也只接上一次', async () => {
    backend.treeLimit = 2
    backend.objects.push(cacheObject(108, 2, '/pool/main/z.deb'))
    renderCache('/admin/cache?path=deb.debian.org/pool/main')
    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('g/')
    const hold = holdFetch((url) => url.includes('offset=2'))

    await userEvent.click(within(table).getByRole('button', { name: '加载更多' }))
    await userEvent.click(within(table).getByRole('button', { name: '加载更多' }))
    await vi.waitFor(() => expect(hold.held).toBe(2))
    hold.release()
    await vi.waitFor(() => expect(hold.settled).toBe(2))
    await flush()

    expect(within(table).getAllByText('z.deb')).toHaveLength(1)
  })

  it('同一层先后问了两次，先发的那次后回来时不盖掉后发的那次', async () => {
    renderCache('/admin/cache?path=deb.debian.org')
    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('dists/')
    const dists = `path=${encodeURIComponent('deb.debian.org/dists')}`
    // 只扣第一次：它问到的是 dists 还没被清掉时的库。
    const hold = holdFetch((url) => url.includes(dists), 1)

    await userEvent.click(within(table).getByRole('button', { name: '展开 dists' }))
    await vi.waitFor(() => expect(hold.held).toBe(1))
    await userEvent.click(within(table).getByRole('button', { name: '收起 dists' }))
    backend.objects = backend.objects.filter((object) => !object.key.startsWith('/dists/'))
    await userEvent.click(within(table).getByRole('button', { name: '展开 dists' }))
    await flush()
    expect(within(table).queryByText('bookworm/')).not.toBeInTheDocument()

    hold.release()
    await vi.waitFor(() => expect(hold.settled).toBe(1))
    await flush()

    expect(within(table).queryByText('bookworm/')).not.toBeInTheDocument()
  })

  it('地址指向一个已经不存在的目录时给空状态，路径导航仍能点回上层', async () => {
    renderCache('/admin/cache?path=deb.debian.org/gone')

    expect(await screen.findByText('这个目录下没有缓存对象。')).toBeInTheDocument()
    const crumbs = screen.getByRole('navigation', { name: '目录路径' })
    await userEvent.click(within(crumbs).getByRole('button', { name: 'deb.debian.org' }))

    const table = screen.getByRole('table', { name: '缓存对象' })
    expect(await within(table).findByText('pool/')).toBeInTheDocument()
  })

  it('一层读不出来时给错误提示，不当成空目录，也不报出 0 个对象', async () => {
    backend.failTree = true
    renderCache('/admin/cache?path=docker.io')

    expect(await screen.findByRole('alert')).toHaveTextContent('这一层没有读出来，稍后刷新再试。')
    expect(screen.queryByText('这个目录下没有缓存对象。')).not.toBeInTheDocument()
    expect(screen.queryByText(/个对象 · /)).not.toBeInTheDocument()
    // 路径导航仍能点回上层，回到读得出来的那一层后错误提示收起。
    const crumbs = screen.getByRole('navigation', { name: '目录路径' })
    await userEvent.click(within(crumbs).getByRole('button', { name: '全部上游' }))
    const table = screen.getByRole('table', { name: '缓存对象' })
    expect(await within(table).findByText('docker.io/')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('就地展开的那一层读不出来时同样给错误提示', async () => {
    backend.failTree = true
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('deb.debian.org/')
    const deb = treeRow(table, /deb\.debian\.org\//)
    await userEvent.click(within(deb).getByRole('button', { name: '展开 deb.debian.org' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('这一层没有读出来，稍后刷新再试。')
    await userEvent.click(within(deb).getByRole('button', { name: '收起 deb.debian.org' }))
    await vi.waitFor(() => {
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    })
  })

  it('清除一个对象走单条清除，不要确认', async () => {
    renderCache('/admin/cache?path=docker.io/library/redis/manifests')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const row = await within(table).findByRole('row', { name: /@9f3a2c/ })
    await userEvent.click(within(row).getByRole('button', { name: '清除' }))

    await vi.waitFor(() => {
      expect(within(table).queryByText('@9f3a2c')).not.toBeInTheDocument()
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
    renderCache('/admin/cache?path=deb.debian.org/pool/main/g/glibc')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const row = await within(table).findByRole('row', { name: /libc6/ })
    await userEvent.click(within(row).getByRole('checkbox'))
    expect(screen.getByText('已选 1 项')).toBeInTheDocument()

    await userEvent.click(await screen.findByRole('button', { name: '取消固定' }))

    const pin = calls.find((call) => call.url.endsWith('/pin'))
    expect(pin?.url).toContain('/admin/cache/objects/102/pin')
    expect(pin?.body).toEqual({ pinned: false })
  })

  it('展开出来的对象行也能勾选批量清除', async () => {
    renderCache('/admin/cache?path=docker.io/library/redis')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const manifests = await within(table).findByRole('row', { name: /manifests\// })
    await userEvent.click(within(manifests).getByRole('button', { name: '展开 manifests' }))
    const rows = await within(table).findAllByRole('row', { name: /^选择.*7/ })
    await userEvent.click(within(rows[0]).getByRole('checkbox'))
    await userEvent.click(within(rows[1]).getByRole('checkbox'))
    const toolbar = screen.getByText('已选 2 项').parentElement as HTMLElement
    await userEvent.click(within(toolbar).getByRole('button', { name: '清除' }))

    await vi.waitFor(() => {
      expect(within(table).queryByRole('row', { name: /^选择 docker\.io/ })).not.toBeInTheDocument()
    })
    const ids = calls
      .filter((call) => call.url === '/api/v1/admin/cache/purge')
      .map((call) => call.body.id)
    expect(ids.sort()).toEqual([101, 106])
  })

  it('搜索在当前目录下进行，结果按树显示：上级目录展开、匹配片段高亮、写明匹配数', async () => {
    renderCache('/admin/cache?path=deb.debian.org')

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('pool/')
    const input = screen.getByLabelText('搜索缓存对象')
    expect(input).toHaveAttribute('placeholder', '在当前目录下搜索路径')
    await userEvent.type(input, 'REDIS')

    await within(table).findByRole('row', { name: /redis-server_7\.0\.15_amd64\.deb/ })
    await vi.waitFor(() => {
      expect(within(table).queryByText('dists/')).not.toBeInTheDocument()
    })
    const search = calls.find((call) => call.url.startsWith('/api/v1/admin/cache/tree/search?'))
    expect(new URLSearchParams(search?.url.split('?')[1]).get('path')).toBe('deb.debian.org')
    expect(screen.getByText('命中 1 个')).toBeInTheDocument()

    // 上级目录 pool/ main/ r/ redis/ 全部展开；名称本身匹配的目录标出匹配数。
    const redisDir = treeRow(table, /redis\/.*匹配 1/)
    expect(within(redisDir).getByText('redis', { selector: 'mark' })).toBeInTheDocument()
    expect(within(table).getByText('main/')).toBeInTheDocument()
    expect(within(table).queryByText('glibc/')).not.toBeInTheDocument()
    const leaf = treeRow(table, /redis-server/)
    expect(within(leaf).getByText('redis', { selector: 'mark' })).toBeInTheDocument()

    await userEvent.clear(input)
    expect(await within(table).findByText('dists/')).toBeInTheDocument()
    expect(screen.getByTestId('location')).toHaveAttribute('data-search', '?path=deb.debian.org')
  })

  it('匹配超出上限时只显示前一批，并提示输入更具体的路径', async () => {
    backend.searchLimit = 1
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('docker.io/')
    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'redis')

    expect(
      await screen.findByText('只显示了前 1 个匹配，输入更具体的路径或先进入目录。')
    ).toBeInTheDocument()
    expect(screen.getByText('命中 4 个')).toBeInTheDocument()
  })

  it('搜不到时给空状态', async () => {
    renderCache()

    await screen.findByRole('table', { name: '缓存对象' })
    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'nothing-here')

    expect(await screen.findByText('没有匹配的对象。')).toBeInTheDocument()
  })

  it('搜索出错时给错误提示，不当成搜不到', async () => {
    backend.failTreeSearch = true
    renderCache()

    await screen.findByRole('table', { name: '缓存对象' })
    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'redis')

    expect(await screen.findByRole('alert')).toHaveTextContent('搜索没有完成，稍后再试。')
    expect(screen.queryByText('没有匹配的对象。')).not.toBeInTheDocument()
    expect(screen.queryByText(/^命中 /)).not.toBeInTheDocument()
  })

  it('按目录清除要先确认，确认处写明路径与未固定的对象数，完成后报出清除与跳过的条数', async () => {
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    const deb = await within(table).findByRole('row', { name: /deb\.debian\.org\// })
    await userEvent.click(within(deb).getByRole('button', { name: '清除' }))

    const confirm = await screen.findByRole('alertdialog')
    expect(confirm).toHaveTextContent('清除 deb.debian.org 下 2 个未固定的对象？')
    expect(calls.some((call) => call.url === '/api/v1/admin/cache/tree/purge')).toBe(false)

    await userEvent.click(within(confirm).getByRole('button', { name: '确认清除' }))

    expect(await screen.findByRole('status')).toHaveTextContent('清除 2 个，跳过 1 个已固定')
    const purge = calls.find((call) => call.url === '/api/v1/admin/cache/tree/purge')
    expect(purge?.body).toEqual({ path: 'deb.debian.org' })
    // 刷新当前视图：deb.debian.org 只剩那一个固定的对象。
    await vi.waitFor(() => {
      expect(within(treeRow(table, /deb\.debian\.org\//)).getByText('1')).toBeInTheDocument()
    })
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('搜索结果里的目录也能按目录清除，确认处的数不含已固定的对象', async () => {
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('docker.io/')
    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'glibc')
    const glibc = await within(table).findByRole('row', { name: /glibc\/.*匹配 1/ })
    await userEvent.click(within(glibc).getByRole('button', { name: '清除' }))

    // 匹配到 1 个，但它是固定的：将清除的是 0 个。
    expect(await screen.findByRole('alertdialog')).toHaveTextContent(
      '清除 deb.debian.org/pool/main/g/glibc 下 0 个未固定的对象？'
    )
  })

  it('搜索结果里的目录清除前问不到合计时给错误提示，不停在一个点不了的确认上', async () => {
    backend.failTreeAfterSearch = true
    renderCache()

    const table = await screen.findByRole('table', { name: '缓存对象' })
    await within(table).findByText('docker.io/')
    await userEvent.type(screen.getByLabelText('搜索缓存对象'), 'glibc')
    const glibc = await within(table).findByRole('row', { name: /glibc\/.*匹配 1/ })
    await userEvent.click(within(glibc).getByRole('button', { name: '清除' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('这一层没有读出来，稍后刷新再试。')
  })

  it('路径导航旁的「清除本目录」在根上不出现', async () => {
    renderCache()
    await screen.findByRole('table', { name: '缓存对象' })
    expect(screen.queryByRole('button', { name: '清除本目录' })).not.toBeInTheDocument()
  })

  it('清除本目录：取消不发请求，后端拒绝时说翻译过的话', async () => {
    backend.failTreePurge = true
    renderCache('/admin/cache?path=docker.io/library')

    await userEvent.click(await screen.findByRole('button', { name: '清除本目录' }))
    const confirm = await screen.findByRole('alertdialog')
    expect(confirm).toHaveTextContent('清除 docker.io/library 下 3 个未固定的对象？')
    await userEvent.click(within(confirm).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(calls.some((call) => call.url === '/api/v1/admin/cache/tree/purge')).toBe(false)

    await userEvent.click(screen.getByRole('button', { name: '清除本目录' }))
    await userEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: '确认清除' })
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('目录路径不合法，刷新一下再试。')
    expect(screen.queryByText(/cache tree path invalid/)).not.toBeInTheDocument()
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
})
