import { useEffect, useState } from 'react'

// 全局轻提示（替代 alert / confirm）
//
// 背景：审计发现 6 处 alert()/confirm()，在深色界面里弹原生白框，
// 长错误无法复制、阻塞主线程；而 index.css 里早有 .fe-toast 样式却零调用（死代码）。
// 这里用模块级订阅实现，不引入 context，任何文件 import { toast } 即可。

export type ToastKind = 'info' | 'success' | 'error'

export interface ToastItem {
  id: number
  message: string
  kind: ToastKind
}

type Listener = (item: ToastItem) => void

let seq = 0
const listeners = new Set<Listener>()

export function toast(message: string, kind: ToastKind = 'info', ms = 4000) {
  const item: ToastItem = { id: ++seq, message, kind }
  listeners.forEach((l) => l(item))
  if (ms > 0) {
    // 自动消失由 Host 负责（它持有卸载逻辑），这里只负责广播
  }
  return item.id
}

/** 挂载一次即可（App 根节点）；渲染在右下角，不阻塞交互 */
export default function ToastHost() {
  const [items, setItems] = useState<ToastItem[]>([])

  useEffect(() => {
    const onItem: Listener = (item) => {
      setItems((prev) => [...prev.slice(-3), item]) // 最多同时 4 条
      window.setTimeout(() => {
        setItems((prev) => prev.filter((i) => i.id !== item.id))
      }, 5000)
    }
    listeners.add(onItem)
    return () => {
      listeners.delete(onItem)
    }
  }, [])

  if (items.length === 0) return null

  return (
    <div className="fixed bottom-4 right-4 z-[80] flex flex-col gap-2 pointer-events-none">
      {items.map((it) => (
        <div
          key={it.id}
          className={`fe-toast pointer-events-auto max-w-[420px] px-3.5 py-2.5 rounded-lg border text-[12px] leading-relaxed shadow-lg ${
            it.kind === 'error'
              ? 'border-error/50 bg-error/15 text-error'
              : it.kind === 'success'
                ? 'border-success/50 bg-success/15 text-success'
                : 'border-border bg-surface text-gray-200'
          }`}
          role="status"
          aria-live="polite"
        >
          <div className="flex items-start gap-2">
            <span className="flex-1 whitespace-pre-wrap break-words">{it.message}</span>
            <button
              onClick={() => setItems((prev) => prev.filter((i) => i.id !== it.id))}
              className="text-[11px] opacity-60 hover:opacity-100 px-1"
              aria-label="关闭提示"
            >
              ×
            </button>
          </div>
        </div>
      ))}
    </div>
  )
}
