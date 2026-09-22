/**
 * 静态上游的缓存策略预设。
 *
 * 预设只往表单草稿里填一份普通的 `immutable_patterns`：上游记录始终是唯一真相，
 * 后端不为 docker.io、deb.debian.org 这些具体 host 写任何分支（spec 决策 1、2）。
 * 保存出去的也只有模式数组——没有预设名称，将来调整预设不会静默改掉已经存下的
 * 上游。默认 TTL 是全局设置，这里不碰。
 */

export type UpstreamPresetId = 'custom' | 'apt' | 'go' | 'git' | 'pypi'

export interface UpstreamPreset {
  id: UpstreamPresetId
  patterns: string[]
}

/** 「自定义」不替换当前模式；新建上游时草稿本来就是空的。 */
export const CUSTOM_PRESET: UpstreamPresetId = 'custom'

/**
 * 与 spec「缓存策略表单」那张表逐字对应。改这里等于改所有新登记上游的预设值；
 * 已经保存的上游不会跟着变，除非使用者重新选一次。
 *
 * Go proxy 不能用 `/@v/` 这种宽模式：它同时命中会变的 `@v/list`。版本文件各写
 * 一条，`@v/list` 与 `@latest` 就留在可变档（spec 问题 3）。
 */
export const UPSTREAM_PRESETS: UpstreamPreset[] = [
  { id: CUSTOM_PRESET, patterns: [] },
  { id: 'apt', patterns: ['/pool/'] },
  { id: 'go', patterns: ['/@v/*.info', '/@v/*.mod', '/@v/*.zip'] },
  { id: 'git', patterns: ['/????????????????????????????????????????/'] },
  {
    id: 'pypi',
    patterns: ['/packages/??/??/????????????????????????????????????????????????????????????????/'],
  },
]

/** 预设对应的模式表。未登记或「自定义」都返回空表。 */
export function presetPatterns(id: UpstreamPresetId): string[] {
  return UPSTREAM_PRESETS.find((preset) => preset.id === id)?.patterns ?? []
}

/**
 * 编辑已有上游时，模式与某个预设**逐项完全相同**才显示那个预设，否则显示
 * 「自定义」。顺序也算不同：数组顺序不同就是另一份模式，不能替使用者猜。
 */
export function matchPreset(patterns: string[]): UpstreamPresetId {
  const found = UPSTREAM_PRESETS.find(
    (preset) => preset.id !== CUSTOM_PRESET && samePatterns(preset.patterns, patterns)
  )
  return found ? found.id : CUSTOM_PRESET
}

/**
 * 保存前逐行归一化：去掉空行与每行首尾空白，重复项只保留第一次出现的位置。
 * 归一化后的数组就是请求体里的 `immutable_patterns`。
 */
export function normalizePatterns(lines: string[]): string[] {
  const seen = new Set<string>()
  const patterns: string[] = []
  for (const line of lines) {
    const pattern = line.trim()
    if (pattern === '' || seen.has(pattern)) {
      continue
    }
    seen.add(pattern)
    patterns.push(pattern)
  }
  return patterns
}

/**
 * TTL 是非负整数秒；`0` 表示沿用全局默认 TTL。看不懂或超出安全整数范围时返回
 * null，调用方据此拦住保存——不能把负数或 `NaN` 发给后端。
 */
export function parseMutableTTL(value: string): number | null {
  if (!/^\d+$/.test(value.trim())) {
    return null
  }
  const seconds = Number(value.trim())
  return Number.isSafeInteger(seconds) ? seconds : null
}

function samePatterns(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((pattern, index) => pattern === b[index])
}
