/**
 * 把一条事件翻成界面能说的话。
 *
 * 后端给的是稳定枚举（kind、actor）加一个结构化的 detail，**刻意不给句子**：
 * 一句拼好的英文话既翻译不了也筛选不了。所以这里只决定「用哪条文案、填哪些字段」，
 * 真正的措辞在两份语言资源里，由组件用 t() 取。
 *
 * 认不出来的 kind 也必须有出路：后端将来加了新类别，界面宁可说「一条没见过的
 * 事件」，也不能把 `upstream_degraded` 这种枚举原样贴给用户。
 */

import type { EventItem } from './api'
import { formatBytes, formatCount } from './format'

/** 事件的语气，决定那枚标签长什么样。 */
export type EventTone = 'change' | 'degraded' | 'recovered' | 'reclaim' | 'unknown'

export interface EventView {
  tone: EventTone
  /** 文案的 i18n 键，认不出的 kind 给 unknown。 */
  key: string
  /** 直接填进文案的机器串与数字（主机名、规则 pattern、字节数）。 */
  values: Record<string, string | number>
  /** 值本身也是后端枚举、还要再翻一层的字段（规则动作）。 */
  enums: Record<string, string>
}

const TONES: Record<string, EventTone> = {
  upstream_degraded: 'degraded',
  upstream_recovered: 'recovered',
  cache_reclaimed: 'reclaim',
  git_mirror_failed: 'degraded',
  git_mirror_rejected: 'degraded',
  // 淘汰和缓存回收同一种语气：都是「盘满了，收走了一些」，不是出了故障。
  git_mirror_evicted: 'reclaim',
}

/** 后端 event_entity 里的全部 kind。不在这张表里的一律走 unknown。 */
const KNOWN_KINDS = new Set([
  'upstream_created',
  'upstream_updated',
  'upstream_deleted',
  'rule_created',
  'rule_updated',
  'rule_deleted',
  'setting_changed',
  'admin_key_rotated',
  'upstream_degraded',
  'upstream_recovered',
  'cache_reclaimed',
  'git_mirror_pending',
  'git_mirror_ready',
  'git_mirror_failed',
  'git_mirror_rejected',
  'git_mirror_evicted',
])

/** 后端 event_entity 的 actor 只有这两个取值，别的一律当未知。 */
const KNOWN_ACTORS = new Set(['admin', 'system'])

function str(detail: Record<string, unknown>, field: string): string {
  const value = detail[field]
  return typeof value === 'string' ? value : ''
}

function num(detail: Record<string, unknown>, field: string): number {
  const value = detail[field]
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

/** 事件属于哪种语气，决定标签的颜色与措辞。 */
export function eventTone(kind: string): EventTone {
  if (!KNOWN_KINDS.has(kind)) {
    return 'unknown'
  }
  return TONES[kind] ?? 'change'
}

/** actor 的 i18n 键。认不出的操作人给 unknown，不把枚举贴出去。 */
export function actorKey(actor: string): string {
  return `admin.events.actor.${KNOWN_ACTORS.has(actor) ? actor : 'unknown'}`
}

/**
 * 一条事件该怎么说。
 *
 * detail 的字段随 kind 而定（见后端各个记录点），取不到就退到一条不带字段的文案：
 * 「删过一条规则」比「删除规则 undefined」有用。
 */
export function describeEvent(event: EventItem): EventView {
  const detail = event.detail ?? {}
  const tone = eventTone(event.kind)
  const view = (
    key: string,
    values: Record<string, string | number> = {},
    enums: Record<string, string> = {}
  ): EventView => ({
    tone,
    key: `admin.events.kind.${key}`,
    values,
    enums,
  })

  switch (event.kind) {
    case 'upstream_created':
    case 'upstream_updated':
    case 'upstream_deleted':
    case 'upstream_degraded':
    case 'upstream_recovered': {
      const host = str(detail, 'host')
      return host ? view(event.kind, { host }) : view(`${event.kind}_unknown`)
    }
    case 'rule_created':
    case 'rule_updated':
    case 'rule_deleted': {
      const pattern = str(detail, 'pattern')
      const action = str(detail, 'action')
      if (!pattern || !action) {
        return view(`${event.kind}_unknown`)
      }
      return view(event.kind, { pattern }, { action: `admin.events.action.${action}` })
    }
    case 'setting_changed': {
      const keys = Array.isArray(detail.keys)
        ? detail.keys.filter((key): key is string => typeof key === 'string')
        : []
      return keys.length > 0
        ? view(event.kind, { keys: keys.join(' · ') })
        : view(`${event.kind}_unknown`)
    }
    case 'admin_key_rotated':
      return view(event.kind)
    case 'git_mirror_pending':
    case 'git_mirror_rejected': {
      const host = str(detail, 'host')
      const repo = str(detail, 'repo')
      return host && repo ? view(event.kind, { host, repo }) : view(`${event.kind}_unknown`)
    }
    case 'git_mirror_ready': {
      const host = str(detail, 'host')
      const repo = str(detail, 'repo')
      if (!host || !repo) {
        return view(`${event.kind}_unknown`)
      }
      const size = formatBytes(num(detail, 'size_bytes'))
      return view(event.kind, { host, repo, size: `${size.value} ${size.unit}` })
    }
    case 'git_mirror_failed': {
      const host = str(detail, 'host')
      const repo = str(detail, 'repo')
      const error = str(detail, 'error')
      if (!host || !repo || !error) {
        return view(`${event.kind}_unknown`)
      }
      return view(event.kind, { host, repo, error })
    }
    case 'git_mirror_evicted': {
      const host = str(detail, 'host')
      const repo = str(detail, 'repo')
      if (!host || !repo) {
        return view(`${event.kind}_unknown`)
      }
      const size = formatBytes(num(detail, 'size_bytes'))
      return view(event.kind, { host, repo, size: `${size.value} ${size.unit}` })
    }
    case 'cache_reclaimed': {
      const freed = formatBytes(num(detail, 'freed_bytes'))
      return view(event.kind, {
        removed: formatCount(num(detail, 'removed')),
        size: `${freed.value} ${freed.unit}`,
      })
    }
    default:
      return view('unknown')
  }
}
