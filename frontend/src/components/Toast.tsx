import { useEffect, useState } from 'react'

// 全局轻提示 + 确认框（替代 alert / confirm）
//
// 模块级订阅，不引入 context：任何文件 `import { toast, confirmDialog }` 即可。
// 样式走 index.css 的 .toast / .confirm（不再依赖工具类，主题切换自动跟随）。

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
        <div className="toast-host" aria-live="polite">
          {items.map((it) => (
            <div key={it.id} className={`toast toast--${it.kind}`} role="status">
              <span className="toast__text">{it.message}</span>
              <button
                onClick={() => setItems((prev) => prev.filter((i) => i.id !== it.id))}
                className="toast__x"
                aria-label="关闭提示"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      )}

      {req && (
        <div className="modal-backdrop" role="dialog" aria-modal="true" aria-label={req.title} onClick={() => answer(false)}>
          <div className="confirm" onClick={(e) => e.stopPropagation()}>
            <p className="confirm__title">{req.title}</p>
            <p className="confirm__msg">{req.message}</p>
            <div className="confirm__actions">
              <button onClick={() => answer(false)} className="btn btn--ghost" autoFocus>
                取消
              </button>
              <button
                onClick={() => answer(true)}
                className={`btn ${req.danger ? 'btn--danger' : 'btn--primary'}`}
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
