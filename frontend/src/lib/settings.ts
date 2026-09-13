/**
 * 运行时设置的读取。
 *
 * 后端给的是一张键值表（`{key, value, type}`），因为设置项本来就是键值形态——
 * 固定结构体遇到没见过的字段只会安静忽略。界面这一侧的代价是：取一项值要按键找，
 * 而且**必须**认类型。所以读取集中在这里，各屏幕不再自己 `as number`：
 * 一个把字符串当数用的地方，界面上看不出来，保存一次才会发现值被改坏了。
 *
 * 取值范围不在这里复制一份：哪个键合法、每个键的上下界是后端那张 settingDefs
 * 说了算（它同时守着拉取路径），前端再抄一份就会有两套说法。越界由后端拒绝，
 * 界面按错误码说人话。
 */

import type { SettingItem } from './api'

function find(list: SettingItem[] | null, key: string): SettingItem | undefined {
  return list?.find((item) => item.key === key)
}

/** 读一项整数设置，读不到或类型不对时给 0。 */
export function readIntSetting(list: SettingItem[] | null, key: string): number {
  const value = find(list, key)?.value
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

/** 读一项字符串设置，读不到或类型不对时给空串。 */
export function readStringSetting(list: SettingItem[] | null, key: string): string {
  const value = find(list, key)?.value
  return typeof value === 'string' ? value : ''
}

/** 读一项开关设置，读不到或类型不对时给 false。 */
export function readBoolSetting(list: SettingItem[] | null, key: string): boolean {
  return find(list, key)?.value === true
}
