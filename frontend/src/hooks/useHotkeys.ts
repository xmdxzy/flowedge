import { useEffect } from 'react'

interface HotkeyOptions {
  onRun?: () => void
  onSave?: () => void
  onDelete?: () => void
  onUndo?: () => void
  onRedo?: () => void
}

export function useHotkeys(options: HotkeyOptions) {
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      const isMac = navigator.platform.toUpperCase().includes('MAC')
      const modKey = isMac ? e.metaKey : e.ctrlKey

      // Cmd/Ctrl + Enter = 运行
      if (modKey && e.key === 'Enter') {
        e.preventDefault()
        options.onRun?.()
      }

      // Cmd/Ctrl + S = 保存
      if (modKey && e.key === 's') {
        e.preventDefault()
        options.onSave?.()
      }

      // Delete / Backspace = 删除选中
      if (e.key === 'Delete' || e.key === 'Backspace') {
        const target = e.target as HTMLElement
        if (target.tagName !== 'INPUT' && target.tagName !== 'TEXTAREA') {
          options.onDelete?.()
        }
      }

      // Cmd/Ctrl + Z = 撤销
      if (modKey && e.key === 'z' && !e.shiftKey) {
        e.preventDefault()
        options.onUndo?.()
      }

      // Cmd/Ctrl + Shift + Z = 重做
      if (modKey && e.key === 'z' && e.shiftKey) {
        e.preventDefault()
        options.onRedo?.()
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [options])
}
