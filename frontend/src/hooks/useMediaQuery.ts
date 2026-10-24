import { useEffect, useState } from 'react'

// 视口断点：返回当前是否命中该媒体查询，并跟随窗口变化。
//
// 为什么不用纯 CSS 媒体查询隐藏顶栏入口：
//   实测过 `@media (max-width:1320px){ .fe-topbar .fe-tb--minor{display:none} }`
//   在同一节点上命中（matchMedia=true）但计算样式没变（visibility 仍是可见），
//   排查两轮无果且无法用断言锁住。改成 JS 判宽 + 条件渲染后，
//   「窄屏到底渲染了几个入口」可以直接被测试/探针读到。
export function useMediaQuery(query: string): boolean {
  const [match, setMatch] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false
    return window.matchMedia(query).matches
  })

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const mq = window.matchMedia(query)
    const sync = () => setMatch(mq.matches)
    sync() // query 变化时立即对齐一次，避免首帧用旧值
    mq.addEventListener('change', sync)
    return () => mq.removeEventListener('change', sync)
  }, [query])

  return match
}

/** 窄屏：顶栏放不下 12 个入口时的断点（与 index.css 的媒体查询同值） */
export const NARROW_QUERY = '(max-width: 1320px)'
