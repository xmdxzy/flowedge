import { useEffect, useRef } from 'react'

interface HotkeyOptions {
  onRun?: () => void
  onSave?: () => void
  onDelete?: () => void
  onUndo?: () => void
  onRedo?: () => void
}

export function useHotkeys(options: HotkeyOptions) {
  // 用 ref 持有最新回调，依赖数组为空 → 监听器只绑定一次。
  // 原实现把 options 放进依赖数组，导致每次渲染都 add/removeEventListener。
  const ref = useRef(options)
  ref.current = options

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      const isMac = navigator.platform.toUpperCase().includes('MAC')
      const modKey = isMac ? e.metaKey : e.ctrlKey
      const key = e.key.toLowerCase()

      // Cmd/Ctrl + Enter = 运行
      if (modKey && e.key === 'Enter') {
        e.preventDefault()
        ref.current.onRun?.()
      }

      // Cmd/Ctrl + S = 保存
      if (modKey && key === 's') {
        e.preventDefault()
        ref.current.onSave?.()
      }

      // Delete / Backspace = 删除选中（输入态不拦截）
      if (e.key === 'Delete' || e.key === 'Backspace') {
        const target = e.target as HTMLElement
        if (target.tagName !== 'INPUT' && target.tagName !== 'TEXTAREA') {
          ref.current.onDelete?.()
        }
      }

      // Cmd/Ctrl + Z = 撤销
      if (modKey && key === 'z' && !e.shiftKey) {
        e.preventDefault()
        ref.current.onUndo?.()
      }

      // Cmd/Ctrl + Shift + Z 或 Ctrl + Y = 重做
      if (modKey && ((key === 'z' && e.shiftKey) || key === 'y')) {
        e.preventDefault()
        ref.current.onRedo?.()
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [])
}
