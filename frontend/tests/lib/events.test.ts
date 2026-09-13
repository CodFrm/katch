import { describe, expect, it } from 'vitest'

import type { EventItem } from '@/lib/api'
import { actorKey, describeEvent, eventTone } from '@/lib/events'

function event(patch: Partial<EventItem>): EventItem {
  return {
    id: 1,
    kind: 'upstream_created',
    actor: 'admin',
    upstream_id: 0,
    detail: {},
    createtime: 0,
    ...patch,
  }
}

describe('describeEvent', () => {
  // 后端给的是枚举加结构化细节，界面按 kind 查文案键、按字段填空。
  it('把 detail 的字段填进对应的文案键', () => {
    expect(
      describeEvent(event({ kind: 'upstream_degraded', detail: { host: 'pypi.org' } }))
    ).toEqual({
      tone: 'degraded',
      key: 'admin.events.kind.upstream_degraded',
      values: { host: 'pypi.org' },
      enums: {},
    })
  })

  // 规则动作本身也是后端枚举（allow / deny），所以它不是直接填进去的值，
  // 而是再指向一个翻译键——否则界面上会出现一个英文单词。
  it('规则动作指向另一个翻译键，而不是直接当值填进去', () => {
    const view = describeEvent(
      event({ kind: 'rule_created', detail: { action: 'deny', pattern: 'library/alpine:3.21' } })
    )
    expect(view.values).toEqual({ pattern: 'library/alpine:3.21' })
    expect(view.enums).toEqual({ action: 'admin.events.action.deny' })
  })

  // 细节缺字段时退到不带字段的文案：「删除了一条规则」比「删除规则 undefined」有用。
  it('细节缺字段时退到不带字段的那条文案', () => {
    expect(describeEvent(event({ kind: 'rule_deleted', detail: {} })).key).toBe(
      'admin.events.kind.rule_deleted_unknown'
    )
    expect(describeEvent(event({ kind: 'upstream_deleted', detail: {} })).key).toBe(
      'admin.events.kind.upstream_deleted_unknown'
    )
    expect(describeEvent(event({ kind: 'setting_changed', detail: { keys: [] } })).key).toBe(
      'admin.events.kind.setting_changed_unknown'
    )
  })

  it('多个设置键连成一串，字节数按人读的写法', () => {
    expect(
      describeEvent(event({ kind: 'setting_changed', detail: { keys: ['cache_quota', 'public'] } }))
        .values
    ).toEqual({ keys: 'cache_quota · public' })
    expect(
      describeEvent(
        event({ kind: 'cache_reclaimed', detail: { removed: 1420, freed_bytes: 152520359936 } })
      ).values
    ).toEqual({ removed: '1,420', size: '142 GB' })
  })

  // 后端将来加了新的 kind，界面宁可说「一条没见过的事件」，也不能把枚举贴出去。
  it('认不出来的 kind 走兜底文案，不回显枚举本身', () => {
    const view = describeEvent(event({ kind: 'meteor_strike' }))
    expect(view.key).toBe('admin.events.kind.unknown')
    expect(view.tone).toBe('unknown')
    expect(JSON.stringify(view)).not.toContain('meteor_strike')
  })
})

describe('eventTone 与 actorKey', () => {
  it('自动事件与人为变更分成不同的语气', () => {
    expect(eventTone('upstream_recovered')).toBe('recovered')
    expect(eventTone('cache_reclaimed')).toBe('reclaim')
    expect(eventTone('setting_changed')).toBe('change')
    expect(eventTone('admin_key_rotated')).toBe('change')
  })

  it('认不出来的操作人也有翻译键', () => {
    expect(actorKey('admin')).toBe('admin.events.actor.admin')
    expect(actorKey('system')).toBe('admin.events.actor.system')
    expect(actorKey('robot')).toBe('admin.events.actor.unknown')
  })
})
