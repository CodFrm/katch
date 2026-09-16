import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import i18n from '@/i18n'
import {
  ADMIN_KEY_STORAGE,
  type AdminUpstreamItem,
  type CacheImageItem,
  type CacheImageTag,
} from '@/lib/api'

// 覆盖任务目标（任务 5）：左栏在「Git 镜像」之后新增「容器镜像」并路由到 /admin/images；
// 搜索、仅 registry 的上游筛选、镜像行展开 tag（摘要/pin/过期/变体数）、加载更多、
// 删除镜像与 tag 带确认并提示清除/跳过条数、空状态。其余管理屏幕已经在
// admin-page.test.tsx / admin-manage.test.tsx / git-mirrors-screen.test.tsx 里覆盖过。

const KEY = 'right-key'
const TO = 1757700000
const MB = 1024 ** 2

interface Call {
  url: string
  method: string
  body: Record<string, unknown>
}

let calls: Call[] = []

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, msg: 'success', data }) }
}

function rejected(code: number, msg: string) {
  return { ok: false, status: 400, json: async () => ({ code, msg, data: null }) }
}

function upstreamRows(): AdminUpstreamItem[] {
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
      host: 'quay.io',
      protocols: ['registry'],
      origin: 'https://quay.io',
      enabled: true,
      immutable_patterns: [],
      mutable_ttl_seconds: 300,
      default_policy: 'allow_all',
      library_completion: false,
      note: '',
      createtime: 0,
      updatetime: 0,
    },
    {
      id: 3,
      host: 'deb.debian.org',
      protocols: ['static'],
      origin: 'https://deb.debian.org',
      enabled: true,
      immutable_patterns: [],
      mutable_ttl_seconds: 600,
      default_policy: 'allow_all',
      library_completion: false,
      note: '',
      createtime: 0,
      updatetime: 0,
    },
  ]
}

/** 一条镜像记录，取值与后端 api/admin.CacheImageItem 一致（tags 由列表接口按需补）。 */
function imageRows(): CacheImageItem[] {
  return [
    {
      upstream_id: 1,
      host: 'docker.io',
      repository: 'library/redis',
      tag_count: 2,
      object_count: 10,
      pinned_count: 1,
      size: 500 * MB,
      hit_count: 42,
      last_access_at: TO - 60,
      tags: [],
    },
    {
      upstream_id: 1,
      host: 'docker.io',
      repository: 'library/nginx',
      tag_count: 0,
      object_count: 4,
      pinned_count: 0,
      size: 100 * MB,
      hit_count: 5,
      last_access_at: TO - 300,
      tags: [],
    },
    {
      upstream_id: 2,
      host: 'quay.io',
      repository: 'prometheus/prometheus',
      tag_count: 1,
      object_count: 6,
      pinned_count: 0,
      size: 200 * MB,
      hit_count: 9,
      last_access_at: TO - 500,
      tags: [],
    },
  ]
}

/** library/redis 的 tag 行：一个多变体、一个已固定、一个按摘要拉取且已过期。 */
function redisTags(): CacheImageTag[] {
  return [
    {
      reference: '7',
      by_digest: false,
      digest: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
      variants: 2,
      object_count: 3,
      pinned_count: 1,
      pinned: true,
      expired: false,
      hit_count: 20,
      last_access_at: TO - 60,
    },
    {
      reference: 'latest',
      by_digest: false,
      digest: 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
      variants: 1,
      object_count: 1,
      pinned_count: 1,
      pinned: true,
      expired: false,
      hit_count: 5,
      last_access_at: TO - 500,
    },
    {
      reference: 'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
      by_digest: true,
      digest: 'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
      variants: 1,
      object_count: 1,
      pinned_count: 0,
      pinned: false,
      expired: true,
      hit_count: 1,
      last_access_at: TO - 900,
    },
  ]
}

function prometheusTags(): CacheImageTag[] {
  return [
    {
      reference: 'v2.53.0',
      by_digest: false,
      digest: 'sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
      variants: 1,
      object_count: 5,
      pinned_count: 0,
      pinned: false,
      expired: false,
      hit_count: 9,
      last_access_at: TO - 500,
    },
  ]
}

function tagKey(upstreamID: number, repository: string) {
  return `${upstreamID}:${repository}`
}

interface Backend {
  upstreams: AdminUpstreamItem[]
  images: CacheImageItem[]
  tags: Record<string, CacheImageTag[]>
  /** 每页给几条，真实后端是 50；调小了才测得到「加载更多」。 */
  imageSize: number
  failPurge: { code: number; msg: string } | null
}

let backend: Backend

function cacheImages(upstreamID: number, keyword: string, offset: number) {
  let list = backend.images.filter((item) => upstreamID === 0 || item.upstream_id === upstreamID)
  if (keyword) {
    const kw = keyword.toLowerCase()
    list = list.flatMap((item) => {
      if (item.repository.toLowerCase().includes(kw)) {
        return [{ ...item, tags: [] }]
      }
      const matched = (backend.tags[tagKey(item.upstream_id, item.repository)] ?? []).filter(
        (tag) => tag.reference.toLowerCase().includes(kw)
      )
      return matched.length > 0 ? [{ ...item, tags: matched }] : []
    })
  }
  const total = list.length
  const page = list.slice(offset, offset + backend.imageSize)
  return {
    total,
    has_more: offset + page.length < total,
    next_offset: offset + page.length,
    list: page,
  }
}

function stubFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {}
      calls.push({ url, method, body })
      const [path, search] = url.split('?')
      const query = new URLSearchParams(search ?? '')

      if (path === '/api/v1/system/version') {
        return envelope({ version: '0.1.0', commit: 'abc1234' })
      }
      if (path === '/api/v1/admin/upstreams') {
        return envelope({ list: backend.upstreams })
      }
      if (path === '/api/v1/admin/stats/upstreams') {
        return envelope({ range: '24h', from: 0, to: TO, list: [] })
      }
      if (path === '/api/v1/admin/cache/images' && method === 'GET') {
        return envelope(
          cacheImages(
            Number(query.get('upstream_id') ?? '0'),
            query.get('keyword') ?? '',
            Number(query.get('offset') ?? '0')
          )
        )
      }
      if (path === '/api/v1/admin/cache/images/tags' && method === 'GET') {
        const key = tagKey(Number(query.get('upstream_id') ?? '0'), query.get('repository') ?? '')
        return envelope({ list: backend.tags[key] ?? [] })
      }
      if (path === '/api/v1/admin/cache/images/purge' && method === 'POST') {
        if (backend.failPurge) {
          return rejected(backend.failPurge.code, backend.failPurge.msg)
        }
        const upstreamID = Number(body.upstream_id ?? 0)
        const repository = String(body.repository ?? '')
        const reference = String(body.reference ?? '')
        const key = tagKey(upstreamID, repository)
        if (reference) {
          const tags = backend.tags[key] ?? []
          const tag = tags.find((item) => item.reference === reference)
          const removed = tag ? tag.object_count - tag.pinned_count : 0
          const skipped = tag ? tag.pinned_count : 0
          backend.tags[key] = tags.filter((item) => item.reference !== reference)
          const image = backend.images.find(
            (item) => item.upstream_id === upstreamID && item.repository === repository
          )
          if (image && tag) {
            image.tag_count -= 1
            image.object_count -= removed
          }
          return envelope({ removed, skipped })
        }
        const image = backend.images.find(
          (item) => item.upstream_id === upstreamID && item.repository === repository
        )
        const removed = image ? image.object_count - image.pinned_count : 0
        const skipped = image ? image.pinned_count : 0
        backend.images = backend.images.filter(
          (item) => !(item.upstream_id === upstreamID && item.repository === repository)
        )
        delete backend.tags[key]
        return envelope({ removed, skipped })
      }
      return { ok: false, status: 404, json: async () => ({}) }
    })
  )
}

/**
 * 把命中 match 的请求扣在半路（先按发出那一刻的数据算好响应），release 之后才放行；
 * limit 是最多扣几次。用来摆出只有时序才碰得到的场面。
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

function renderAdmin(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>
  )
}

async function renderImages() {
  const view = renderAdmin('/admin/images')
  await screen.findByRole('table', { name: '容器镜像' })
  return view
}

beforeEach(async () => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  localStorage.clear()
  localStorage.setItem(ADMIN_KEY_STORAGE, KEY)
  calls = []
  backend = {
    upstreams: upstreamRows(),
    images: imageRows(),
    tags: {
      [tagKey(1, 'library/redis')]: redisTags(),
      [tagKey(2, 'prometheus/prometheus')]: prometheusTags(),
    },
    imageSize: 50,
    failPurge: null,
  }
  await i18n.changeLanguage('zh-CN')
  stubFetch()
})

describe('后台 · 容器镜像', () => {
  it('导航栏在 Git 镜像之后有容器镜像入口', async () => {
    renderAdmin('/admin')
    const gitLink = await screen.findByRole('link', { name: 'Git 镜像' })
    const imagesLink = await screen.findByRole('link', { name: '容器镜像' })
    expect(imagesLink).toHaveAttribute('href', '/admin/images')
    const links = screen.getAllByRole('link')
    expect(links.indexOf(imagesLink)).toBe(links.indexOf(gitLink) + 1)
  })

  it('按仓库列出镜像：主机/仓库名、Tag 数、体积、命中、最后访问', async () => {
    const table = await (async () => {
      await renderImages()
      return screen.getByRole('table', { name: '容器镜像' })
    })()

    const rows = within(table).getAllByRole('row')
    // 一条表头 + 三条镜像。
    expect(rows).toHaveLength(4)
    const redisRow = await within(table).findByRole('row', { name: /docker\.io\/library\/redis/ })
    expect(within(redisRow).getByText('2')).toBeInTheDocument()
    expect(within(redisRow).getByRole('button', { name: '删除' })).toBeInTheDocument()
  })

  it('上游筛选只列协议含 registry 的上游，默认全部；切换后带 upstream_id 重新请求', async () => {
    await renderImages()

    const select = screen.getByLabelText('按上游筛选')
    const options = within(select)
      .getAllByRole('option')
      .map((option) => option.textContent)
    expect(options).toEqual(['全部上游', 'docker.io', 'quay.io'])

    await userEvent.selectOptions(select, 'docker.io')

    await waitFor(() => {
      expect(
        calls.some(
          (item) =>
            item.method === 'GET' &&
            item.url.startsWith('/api/v1/admin/cache/images?') &&
            item.url.includes('upstream_id=1')
        )
      ).toBe(true)
    })
  })

  it('手动展开一个镜像后拉取全部 tag：tag 名、短摘要、命中、最后访问、固定/过期/变体数标记', async () => {
    const table = (await renderImages(), screen.getByRole('table', { name: '容器镜像' }))
    const redisRow = await within(table).findByRole('row', { name: /docker\.io\/library\/redis/ })

    await userEvent.click(within(redisRow).getByRole('button', { name: /展开/ }))

    expect(await within(table).findByRole('row', { name: /^7@/ })).toBeInTheDocument()
    const sevenRow = within(table).getByRole('row', { name: /^7@/ })
    expect(within(sevenRow).getByText(/变体/)).toBeInTheDocument()

    const latestRow = within(table).getByRole('row', { name: /latest/ })
    expect(within(latestRow).getByText('已固定')).toBeInTheDocument()

    const digestRow = within(table).getByRole('row', { name: /@cccccc/ })
    expect(within(digestRow).getByText(/已过期/)).toBeInTheDocument()

    const call = calls.find((item) => item.url.startsWith('/api/v1/admin/cache/images/tags?'))
    expect(call?.url).toContain('repository=library%2Fredis')
  })

  it('搜索命中 tag 时该镜像自动展开且只显示命中的 tag', async () => {
    await renderImages()

    await userEvent.type(screen.getByLabelText('搜索容器镜像'), 'latest')

    const table = screen.getByRole('table', { name: '容器镜像' })
    await within(table).findByRole('row', { name: /latest/ })
    expect(within(table).queryByRole('row', { name: /^7@/ })).not.toBeInTheDocument()
    expect(calls.some((item) => item.url.startsWith('/api/v1/admin/cache/images/tags?'))).toBe(
      false
    )
  })

  it('搜索命中仓库名时该镜像自动展开并显示全部 tag', async () => {
    await renderImages()

    await userEvent.type(screen.getByLabelText('搜索容器镜像'), 'redis')

    const table = screen.getByRole('table', { name: '容器镜像' })
    await within(table).findByRole('row', { name: /^7@/ })
    expect(within(table).getByRole('row', { name: /latest/ })).toBeInTheDocument()
    expect(within(table).queryByRole('row', { name: /nginx/ })).not.toBeInTheDocument()
  })

  it('每页超出时显示加载更多，点击后追加下一批', async () => {
    backend.imageSize = 2
    await renderImages()

    const table = screen.getByRole('table', { name: '容器镜像' })
    await within(table).findByRole('row', { name: /nginx/ })
    expect(within(table).queryByRole('row', { name: /prometheus/ })).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '加载更多' }))

    expect(await within(table).findByRole('row', { name: /prometheus/ })).toBeInTheDocument()
  })

  it('「加载更多」在下一批回来之前连点两下，下一批也只接上一次', async () => {
    backend.imageSize = 2
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    await within(table).findByRole('row', { name: /nginx/ })
    const hold = holdFetch((url) => url.includes('offset=2'))

    await userEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await userEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await vi.waitFor(() => expect(hold.held).toBe(2))
    hold.release()
    await vi.waitFor(() => expect(hold.settled).toBe(2))
    await flush()

    expect(within(table).getAllByRole('row', { name: /prometheus/ })).toHaveLength(1)
  })

  it('删除之后的重取先回来、之前发出的「加载更多」后回来时，不把删掉的镜像接回去', async () => {
    backend.imageSize = 2
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    await within(table).findByRole('row', { name: /nginx/ })
    // 下一批按删除之前的库算好，扣住。
    const hold = holdFetch((url) => url.includes('offset=2'), 1)
    await userEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await vi.waitFor(() => expect(hold.held).toBe(1))

    backend.images = backend.images.filter((item) => item.repository !== 'prometheus/prometheus')
    backend.imageSize = 50
    const redisRow = within(table).getByRole('row', { name: /docker\.io\/library\/redis/ })
    await userEvent.click(within(redisRow).getByRole('button', { name: '删除' }))
    await userEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: '确认删除' })
    )
    await waitFor(() => {
      expect(within(table).queryByRole('row', { name: /library\/redis/ })).not.toBeInTheDocument()
    })

    hold.release()
    await vi.waitFor(() => expect(hold.settled).toBe(1))
    await flush()

    expect(within(table).queryByRole('row', { name: /prometheus/ })).not.toBeInTheDocument()
  })

  it('删除镜像要先确认，确认处写明未固定的对象数，完成后提示清除与跳过', async () => {
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    const redisRow = await within(table).findByRole('row', { name: /docker\.io\/library\/redis/ })

    await userEvent.click(within(redisRow).getByRole('button', { name: '删除' }))
    const confirm = await screen.findByRole('alertdialog')
    expect(confirm).toHaveTextContent('9')
    expect(calls.some((item) => item.url === '/api/v1/admin/cache/images/purge')).toBe(false)

    await userEvent.click(within(confirm).getByRole('button', { name: '确认删除' }))

    expect(await screen.findByRole('status')).toHaveTextContent('9')
    expect(await screen.findByRole('status')).toHaveTextContent('1')
    const call = calls.find((item) => item.url === '/api/v1/admin/cache/images/purge')
    expect(call?.body).toEqual({ upstream_id: 1, repository: 'library/redis' })
    await waitFor(() => {
      expect(within(table).queryByRole('row', { name: /library\/redis/ })).not.toBeInTheDocument()
    })
  })

  it('删除单个 tag 要求确认并带上 reference，不影响其它 tag', async () => {
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    const redisRow = await within(table).findByRole('row', { name: /docker\.io\/library\/redis/ })
    await userEvent.click(within(redisRow).getByRole('button', { name: /展开/ }))
    const sevenRow = await within(table).findByRole('row', { name: /^7@/ })

    await userEvent.click(within(sevenRow).getByRole('button', { name: '删除' }))
    const confirm = await screen.findByRole('alertdialog')
    // 决策 11 与决策 6 同一套语义：确认处写明将清除的对象数，不含已固定的那条。
    expect(confirm).toHaveTextContent('删除 tag 7？将清除其下 2 个未固定的对象。')
    await userEvent.click(within(confirm).getByRole('button', { name: '确认删除' }))

    expect(await screen.findByRole('status')).toHaveTextContent('清除 2 个，跳过 1 个已固定')
    const call = calls.find((item) => item.url === '/api/v1/admin/cache/images/purge')
    expect(call?.body).toEqual({ upstream_id: 1, repository: 'library/redis', reference: '7' })
    await waitFor(() => {
      expect(within(table).queryByRole('row', { name: /^7@/ })).not.toBeInTheDocument()
    })
    expect(within(table).getByRole('row', { name: /latest/ })).toBeInTheDocument()
  })

  it('没有跳过任何对象时完成提示也报出跳过条数', async () => {
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    const nginxRow = await within(table).findByRole('row', { name: /docker\.io\/library\/nginx/ })

    await userEvent.click(within(nginxRow).getByRole('button', { name: '删除' }))
    await userEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: '确认删除' })
    )

    expect(await screen.findByRole('status')).toHaveTextContent('清除 4 个，跳过 0 个已固定')
  })

  it('后端拒绝删除时显示对应错误码的翻译文案', async () => {
    backend.failPurge = { code: 10012, msg: 'cache image repository invalid' }
    await renderImages()
    const table = screen.getByRole('table', { name: '容器镜像' })
    const redisRow = await within(table).findByRole('row', { name: /docker\.io\/library\/redis/ })
    await userEvent.click(within(redisRow).getByRole('button', { name: '删除' }))
    await userEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: '确认删除' })
    )

    expect(await screen.findByRole('alert')).toHaveTextContent('仓库名不合法')
    expect(screen.queryByText(/repository invalid/)).not.toBeInTheDocument()
  })

  it('没有镜像缓存时显示空状态', async () => {
    backend.images = []
    await renderImages()
    expect(await screen.findByText('没有镜像缓存。')).toBeInTheDocument()
  })

  it('表格下方常驻体积口径说明', async () => {
    const table = await renderImages().then(() => screen.getByRole('table', { name: '容器镜像' }))
    const note = screen.getAllByText('体积按该仓库下已缓存对象合计，共用层会在各自镜像里各算一次。')
    expect(note.length).toBeGreaterThan(0)
    // 至少有一处出现在表格之后（页头那一份不算「表格下方」）。
    expect(
      note.some((el) => table.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING)
    ).toBe(true)
  })
})
