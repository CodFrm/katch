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
