/** 深浅主题的类名。两套主题共用同一组 CSS 变量，只有取值不同（src/index.css）。 */
const DARK = 'dark'

/**
 * 跟随系统的深浅设置。
 *
 * 没有切换开关：这台镜像站不管理使用者的偏好，系统怎么设它就怎么显示。
 * 返回取消订阅的函数，供调用方在需要时解绑。
 */
export function followColorScheme(): () => void {
  const query = window.matchMedia?.('(prefers-color-scheme: dark)')
  if (!query) {
    return () => {}
  }
  const apply = (dark: boolean) => document.documentElement.classList.toggle(DARK, dark)
  apply(query.matches)
  const onChange = (event: MediaQueryListEvent) => apply(event.matches)
  query.addEventListener('change', onChange)
  return () => query.removeEventListener('change', onChange)
}
