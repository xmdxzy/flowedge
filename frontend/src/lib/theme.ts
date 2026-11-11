// 主题：浅色 / 深色 / 跟随系统。
// 颜色本身由 index.css 的 CSS 变量驱动（见「主题令牌 · 唯一色源」），
// 这里只负责：偏好持久化、解析成实际主题、监听系统切换、切换瞬间免补间。

export type Theme = 'dark' | 'light'
/** 用户偏好；system = 跟随操作系统外观 */
export type ThemePref = Theme | 'system'

const STORAGE_KEY = 'flowedge.theme'
const NO_TRANSITION = 'fe-no-transition'

const prefersLight = () =>
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-color-scheme: light)').matches

export function getInitialPref(): ThemePref {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'light' || saved === 'dark' || saved === 'system') return saved
  } catch {
    /* 隐私模式下 localStorage 可能不可用 */
  }
  // 没存过就跟随系统：系统浅色则给浅色，否则保持深色（深色仍是主视觉）
  return prefersLight() ? 'system' : 'dark'
}

/** 把偏好解析成实际主题 */
export function resolveTheme(pref: ThemePref): Theme {
  if (pref === 'system') return prefersLight() ? 'light' : 'dark'
  return pref
}

/** 兼容旧调用：拿实际主题 */
export function getInitialTheme(): Theme {
  return resolveTheme(getInitialPref())
}

export function applyTheme(pref: ThemePref): Theme {
  const resolved = resolveTheme(pref)
  const root = document.documentElement
  // 偏好每次都落盘：解析后主题没变时（深色系统下点「深色」）也要记住这次选择，
  // 否则下次启动会退回旧偏好，用户会以为「点了没用」。
  try {
    localStorage.setItem(STORAGE_KEY, pref)
  } catch {
    /* 忽略 */
  }
  if (root.getAttribute('data-theme') === resolved) {
    root.setAttribute('data-pref', pref)
    return resolved
  }
  // 切换瞬间关掉过渡：几百个元素同时补间会掉帧，观感反而"糊"
  root.classList.add(NO_TRANSITION)
  root.setAttribute('data-theme', resolved)
  root.setAttribute('data-pref', pref)
  // 两帧之后再恢复过渡，确保新主题已经绘制完
  requestAnimationFrame(() => {
    requestAnimationFrame(() => root.classList.remove(NO_TRANSITION))
  })
  return resolved
}

/** 监听系统外观变化；偏好为 system 时才有意义。返回取消订阅函数。 */
export function watchSystemTheme(onChange: (theme: Theme) => void): () => void {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return () => {}
  const mq = window.matchMedia('(prefers-color-scheme: light)')
  const handler = () => onChange(mq.matches ? 'light' : 'dark')
  mq.addEventListener('change', handler)
  return () => mq.removeEventListener('change', handler)
}

/** 三种偏好在 UI 上的展示顺序与文案 */
export const THEME_OPTIONS: { value: ThemePref; label: string; icon: string; hint: string }[] = [
  { value: 'light', label: '浅色', icon: '☀', hint: '浅色主题' },
  { value: 'dark', label: '深色', icon: '☾', hint: '深色主题' },
  { value: 'system', label: '跟随系统', icon: '⌂', hint: '跟随系统外观自动切换' },
]
