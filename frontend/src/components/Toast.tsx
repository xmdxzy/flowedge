import { useEffect, useState } from 'react'

// 全局轻提示 + 确认框（替代 alert / confirm）
//
// 背景（2026-10-19 审计）：6 处 alert()/confirm() 在深色界面里弹原生白框，
// 长错误无法复制、阻塞主线程；而 index.css 里早有 .fe-toast 样式却零调用（死代码）。
// 这里用模块级订阅实现，不引入 context，任何文件 `import { toast, confirmDialog }` 即可。

export type ToastKind = 'info' | 'success' | 'error'

export interface ToastItem {
  id: number
  message: string
  kind: ToastKind
}

type Listener = (item: ToastItem) => void

let seq = 0
const listeners = new Set<Listener>()

export function toast(message: string, kind: ToastKind = 'info'): number {
  const item: ToastItem = { id: ++seq, message, kind }
  listeners.forEach((l) => l(item))
  return item.id
}

// ---------------------------------------------------------------------------
// 确认框：window.confirm 的替身，返回 Promise<boolean>
// ---------------------------------------------------------------------------

export interface ConfirmRequest {
  id: number
  title: string
  message: string
  danger: boolean
  /** 确认按钮文案 */
  okText: string
  resolve: (v: boolean) => void
}

type ConfirmListener = (r: ConfirmRequest) => void

let cseq = 0
const confirmListeners = new Set<ConfirmListener>()

export function confirmDialog(
  message: string,
  opts: { title?: string; danger?: boolean; okText?: string } = {},
): Promise<boolean> {
  return new Promise((resolve) => {
    // 没有挂载宿主时退回原生 confirm：宁可难看，也不能让「删除」静默失效
    if (confirmListeners.size === 0) {
      resolve(window.confirm(message))
      return
    }
    const req: ConfirmRequest = {
      id: ++cseq,
      title: opts.title ?? '确认操作',
      message,
      danger: opts.danger ?? true,
      okText: opts.okText ?? '确定',
      resolve,
    }
    confirmListeners.forEach((l) => l(req))
  })
}

/** 挂载一次即可（App 根节点）；右下角 toast + 居中确认框，均不阻塞交互 */
export default function ToastHost() {
  const [items, setItems] = useState<ToastItem[]>([])
  const [req, setReq] = useState<ConfirmRequest | null>(null)

  useEffect(() => {
    const onItem: Listener = (item) => {
      setItems((prev) => [...prev.slice(-3), item]) // 最多同时 4 条
      window.setTimeout(() => {
        setItems((prev) => prev.filter((i) => i.id !== item.id))
      }, 5000)
    }
    const onConfirm: ConfirmListener = (r) => setReq(r)
    listeners.add(onItem)
    confirmListeners.add(onConfirm)
    return () => {
      listeners.delete(onItem)
      confirmListeners.delete(onConfirm)
    }
  }, [])

  // Esc 取消确认框
  useEffect(() => {
    if (!req) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        req.resolve(false)
        setReq(null)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [req])

  const answer = (v: boolean) => {
    req?.resolve(v)
    setReq(null)
  }

  return (
    <>
      {items.length > 0 && (
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
      )}

      {req && (
        <div
          className="fixed inset-0 z-[90] flex items-center justify-center bg-black/60"
          role="dialog"
          aria-modal="true"
          aria-label={req.title}
          onClick={() => answer(false)}
        >
          <div
            className="w-[380px] max-w-[92vw] rounded-xl border border-border bg-surface p-4 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <p className="text-sm text-white font-medium">{req.title}</p>
            <p className="text-[12px] text-gray-400 mt-2 leading-relaxed whitespace-pre-wrap break-words">
              {req.message}
            </p>
            <div className="flex justify-end gap-2 mt-4">
              <button
                onClick={() => answer(false)}
                className="text-[12px] px-3 py-1.5 rounded-md border border-border text-gray-300 hover:text-white"
                autoFocus
              >
                取消
              </button>
              <button
                onClick={() => answer(true)}
                className={`text-[12px] px-3 py-1.5 rounded-md border ${
                  req.danger
                    ? 'border-error/50 bg-error/15 text-error hover:bg-error/25'
                    : 'border-primary/50 bg-primary/15 text-primary hover:bg-primary/25'
                }`}
              >
                {req.okText}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}
