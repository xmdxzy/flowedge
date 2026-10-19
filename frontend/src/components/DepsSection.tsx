import { useCallback, useEffect, useRef, useState } from 'react'

interface DepStatus {
  name: string
  label: string
  running: boolean
  managed: boolean
  autostart: boolean
  pid?: number
  endpoint: string
  command?: string
  hint?: string
  last_error?: string
}

interface Props {
  /** 依赖状态变化后通知父级刷新能力矩阵（ComfyUI 起来后 image 就从缺失变可用） */
  onChanged?: () => void
}

// 依赖进程托管：ComfyUI / ASR 一键启停。
// 背景：以前用户必须自己去开终端跑 python main.py，这条链路断了很久。
export default function DepsSection({ onChanged }: Props) {
  const [items, setItems] = useState<DepStatus[]>([])
  const [pending, setPending] = useState<Record<string, boolean>>({})
  const [msg, setMsg] = useState('')
  const timer = useRef<number | null>(null)

  const load = useCallback(async () => {
    try {
      const resp = await fetch('/api/deps')
      const data = await resp.json()
      setItems(Array.isArray(data.items) ? data.items : [])
      return data.items as DepStatus[]
    } catch {
      return []
    }
  }, [])

  useEffect(() => {
    void load()
    return () => {
      if (timer.current) window.clearTimeout(timer.current)
    }
  }, [load])

  // 启动中轮询：直到 running 或出现 last_error
  const pollUntilSettled = useCallback(
    async (name: string) => {
      for (let i = 0; i < 60; i++) {
        await new Promise((r) => setTimeout(r, 2000))
        const list = (await load()) || []
        const dep = list.find((d) => d.name === name)
        if (dep?.running) {
          setPending((p) => ({ ...p, [name]: false }))
          setMsg(`${dep.label} 已就绪`)
          onChanged?.()
          return
        }
        if (dep?.last_error) {
          setPending((p) => ({ ...p, [name]: false }))
          setMsg(`${dep.label} 启动失败：${dep.last_error}`)
          return
        }
      }
      setPending((p) => ({ ...p, [name]: false }))
      setMsg(`${name} 启动超时，请看后端控制台输出`)
    },
    [load, onChanged],
  )

  const start = async (name: string) => {
    setPending((p) => ({ ...p, [name]: true }))
    setMsg(`正在启动 ${name}…（ComfyUI 冷启动约 20–60 秒）`)
    try {
      const resp = await fetch(`/api/deps/${name}/start`, { method: 'POST' })
      if (!resp.ok) {
        const d = await resp.json().catch(() => ({}))
        setPending((p) => ({ ...p, [name]: false }))
        setMsg(d.error || '启动失败')
        return
      }
      void pollUntilSettled(name)
    } catch (e: any) {
      setPending((p) => ({ ...p, [name]: false }))
      setMsg(String(e?.message || e))
    }
  }

  const stop = async (name: string) => {
    const resp = await fetch(`/api/deps/${name}/stop`, { method: 'POST' })
    const d = await resp.json().catch(() => ({}))
    setMsg(resp.ok ? `${name} 已停止` : d.error || '停止失败')
    load()
    onChanged?.()
  }

  if (items.length === 0) {
    return <p className="text-[11px] text-gray-500">后端未提供依赖管理（旧版本引擎）</p>
  }

  return (
    <div className="flex flex-col gap-1.5">
      {items.map((dep) => {
        const busy = pending[dep.name]
        return (
          <div key={dep.name} className="px-3 py-2 rounded-lg bg-canvas border border-border">
            <div className="flex items-center gap-2">
              <div className={`w-2 h-2 rounded-full ${dep.running ? 'bg-success' : 'bg-gray-600'}`} />
              <span className="text-xs text-white">{dep.label}</span>
              <span className="text-[10px] text-gray-500 font-mono">{dep.endpoint}</span>
              {dep.pid ? <span className="text-[10px] text-gray-600">PID {dep.pid}</span> : null}
              <div className="ml-auto flex items-center gap-1.5">
                {dep.running ? (
                  dep.managed ? (
                    <button
                      onClick={() => stop(dep.name)}
                      className="text-[10px] px-2 py-0.5 rounded border border-border text-gray-400 hover:text-error"
                    >
                      停止
                    </button>
                  ) : (
                    <span className="text-[10px] text-success">已就绪（外部进程）</span>
                  )
                ) : (
                  <button
                    onClick={() => start(dep.name)}
                    disabled={busy}
                    className="text-[10px] px-2 py-0.5 rounded bg-primary/20 text-primary border border-primary/40 disabled:opacity-40"
                  >
                    {busy ? '启动中…' : '启动'}
                  </button>
                )}
              </div>
            </div>
            {dep.hint && !dep.running ? (
              <p className="text-[10px] text-gray-500 mt-1 leading-relaxed">{dep.hint}</p>
            ) : null}
            {dep.last_error ? (
              <p className="text-[10px] text-error mt-1 break-all">{dep.last_error}</p>
            ) : null}
          </div>
        )
      })}
      {msg && <p className="text-[10px] text-gray-400 mt-1">{msg}</p>}
    </div>
  )
}
