// 主题：深色（默认）/ 浅色。落 localStorage，刷新后保持。
// 颜色本身由 index.css 的 CSS 变量驱动，这里只负责开关与画布内联色。

export type Theme = 'dark' | 'light'

const STORAGE_KEY = 'flowedge.theme'

export function getInitialTheme(): Theme {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'light' || saved === 'dark') return saved
  } catch {
    /* 隐私模式下 localStorage 可能不可用 */
  }
  return 'dark'
}

export function applyTheme(theme: Theme) {
  document.documentElement.setAttribute('data-theme', theme)
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    /* 忽略 */
  }
}

// React Flow 的画布/小地图颜色是组件 props，没法用 CSS 变量，这里给两套
export const CANVAS_THEME: Record<
  Theme,
  { bg: string; dot: string; minimapBg: string; minimapMask: string; minimapNode: string }
> = {
  dark: {
    bg: '#0A0D14',
    dot: '#1C2436',
    minimapBg: '#0D111B',
    minimapMask: 'rgba(10, 13, 20, 0.75)',
    minimapNode: '#6366F1',
  },
  light: {
    bg: '#F4F6FB',
    dot: '#D2DAE8',
    minimapBg: '#FFFFFF',
    minimapMask: 'rgba(240, 243, 250, 0.72)',
    minimapNode: '#6366F1',
  },
}
