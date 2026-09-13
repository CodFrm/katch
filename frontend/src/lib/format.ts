/**
 * 机器产出的数字怎么写给人看。
 *
 * 值和单位分开返回：侧栏要把 `1.82` 排成 34px 等宽、`TB` 排成 13px，拼成一个
 * 字符串就没法这么排了。表格里需要整串时自己接起来。
 */

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const

export interface FormattedBytes {
  value: string
  unit: string
}

/**
 * 字节数按 1024 进位拆成值与单位。
 *
 * 小于 10 的值给两位小数（`1.82 TB` 比 `2 TB` 有用得多），其余取整——
 * `812.05 GB` 的那两位小数没有任何人在看。
 */
export function formatBytes(bytes: number): FormattedBytes {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return { value: '0', unit: UNITS[0] }
  }
  let value = bytes
  let index = 0
  while (value >= 1024 && index < UNITS.length - 1) {
    value /= 1024
    index += 1
  }
  if (index === 0) {
    return { value: String(Math.round(value)), unit: UNITS[0] }
  }
  return { value: value < 10 ? value.toFixed(2) : String(Math.round(value)), unit: UNITS[index] }
}

/** 0~1 的比值写成一位小数的百分数（不带 `%`，符号由界面自己排）。 */
export function formatPercent(rate: number): string {
  if (!Number.isFinite(rate)) {
    return '0.0'
  }
  return (Math.min(1, Math.max(0, rate)) * 100).toFixed(1)
}

/**
 * 请求数一类的计数：三位一组的整数，不带单位。
 *
 * 不折成「万 / 亿」这种量级单位：那套写法只在中文里成立，翻成英文要换一套进位，
 * 于是同一个数在两种语言下的量级都不一样。分组符固定用 en-US，免得数字本身跟着
 * 浏览器语言变形——它是机器产出的量，不是一句话。
 */
export function formatCount(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '0'
  }
  return Math.round(value).toLocaleString('en-US')
}

/** 事件时刻拆成「哪一天 + 几点几分」，那句「昨天」由界面按自己的语言组织。 */
export interface Stamp {
  day: 'today' | 'yesterday' | 'earlier'
  /** 本地时间的 HH:MM。 */
  time: string
  /** 本地时间的 MM-DD，只在 earlier 时有用。 */
  date: string
}

function pad(value: number): string {
  return String(value).padStart(2, '0')
}

/**
 * 秒级时间戳换成本地时刻。
 *
 * 后端给的是 UTC 秒（时序的桶也一样），换算成看的人所在的时区是界面的事——
 * 同一批数据在两台部署上必须落在同一个小时，显示成几点则各随各地。
 */
export function formatStamp(seconds: number, now: Date = new Date()): Stamp {
  const at = new Date(seconds * 1000)
  const time = `${pad(at.getHours())}:${pad(at.getMinutes())}`
  const date = `${pad(at.getMonth() + 1)}-${pad(at.getDate())}`
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const elapsed = midnight - new Date(at.getFullYear(), at.getMonth(), at.getDate()).getTime()
  const oneDay = 24 * 60 * 60 * 1000
  if (elapsed <= 0) {
    return { day: 'today', time, date }
  }
  if (elapsed <= oneDay) {
    return { day: 'yesterday', time, date }
  }
  return { day: 'earlier', time, date }
}

/**
 * 把 `3 TB` 这样的短串解回字节数，认不出来时给 null。
 *
 * 它是 formatBytes 的逆：设置页把配额读成人能改的短串，再原样写回去。少了这一半，
 * 输入框里显示的和保存下去的就是两个值——那是一种保存一次就改掉设置的界面。
 * 认不出来时**不给一个兜底数**：把 `3 PB?` 当成 0 存进去比报错糟得多。
 */
export function parseBytes(text: string): number | null {
  const matched = /^\s*(\d+(?:\.\d+)?)\s*([a-zA-Z]*)\s*$/.exec(text)
  if (!matched) {
    return null
  }
  const unit = matched[2].toUpperCase() || UNITS[0]
  const index = (UNITS as readonly string[]).indexOf(unit)
  if (index < 0) {
    return null
  }
  return Math.round(Number(matched[1]) * 1024 ** index)
}

/** 秒数写成 `30s` / `5m` / `1h`：能整除的用大单位，不能整除的退回小的。 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return '0s'
  }
  const rounded = Math.round(seconds)
  if (rounded % 3600 === 0) {
    return `${rounded / 3600}h`
  }
  if (rounded % 60 === 0) {
    return `${rounded / 60}m`
  }
  return `${rounded}s`
}

/** `5m` 解回秒数，不带单位按秒算，认不出来给 null。 */
export function parseDuration(text: string): number | null {
  const matched = /^\s*(\d+(?:\.\d+)?)\s*([smh]?)\s*$/.exec(text)
  if (!matched) {
    return null
  }
  const scale = { '': 1, s: 1, m: 60, h: 3600 }[matched[2]] ?? 1
  return Math.round(Number(matched[1]) * scale)
}

/**
 * 秒级时间戳写成本地的 `HH:MM:SS`。
 *
 * 最近请求要精确到秒：同一分钟里拉十几次是常态，只到分钟就看不出先后，而这块
 * 面板的全部用处就是「刚刚按什么顺序发生了什么」。
 */
export function formatClock(seconds: number): string {
  const at = new Date(seconds * 1000)
  return `${pad(at.getHours())}:${pad(at.getMinutes())}:${pad(at.getSeconds())}`
}

/**
 * 一次拉取的耗时：不到一秒的写毫秒，到了一秒的写一位小数的秒。
 *
 * 不统一成秒：命中一份小文件是几毫秒的事，写成 `0.0 s` 等于把「快得看不见」
 * 和「量到了零」说成同一句话，而前者恰恰是缓存在干活的证据。
 */
export function formatLatency(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) {
    return '0 ms'
  }
  if (milliseconds < 1000) {
    return `${Math.round(milliseconds)} ms`
  }
  return `${(milliseconds / 1000).toFixed(1)} s`
}
