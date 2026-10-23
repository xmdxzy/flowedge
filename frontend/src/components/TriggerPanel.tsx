import { useCallback, useEffect, useState } from 'react'

interface TriggerConfig {
  workflow_id: number
  token: string
  cron: string
  enabled: boolean
  cron_description?: string
  webhook_path?: string
  curl_example?: string
  last_triggered_at?: string | null
}

interface Props {
  workflowId?: number
  onClose: () => void
}

const PRESETS = [
  { label: '每分钟', cron: '* * * * *' },
  { label: '每 5 分钟', cron: '*/5 * * * *' },
  { label: '每小时', cron: '0 * * * *' },
  { label: '每天 09:00', cron: '0 9 * * *' },
]

export default function TriggerPanel({ workflowId, onClose }: Props) {
  const [cfg, setCfg] = useState<TriggerConfig | null>(null)
  const [cron, setCron] = useState('')
  const [enabled, setEnabled] = useState(false)
  const [loading, setLoading] = useState(false)
  const [msg, setMsg] = useState('')

  const load = useCallback(async () => {
    if (!workflowId) return
    const resp = await fetch(`/api/workflows/${workflowId}/trigger`)
    if (!resp.ok) return
    const data: TriggerConfig = await resp.json()
    setCfg(data)
    setCron(data.cron || '')
    setEnabled(!!data.enabled)
  }, [workflowId])

  useEffect(() => {
    load()
  }, [load])

  const save = async () => {
    if (!workflowId) return
    setLoading(true)
    setMsg('')
    try {
      const resp = await fetch(`/api/workflows/${workflowId}/trigger`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ cron: cron.trim(), enabled }),
      })
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || '保存失败')
      setCfg(data)
      setMsg('已保存')
    } catch (err: any) {
      setMsg(`失败：${err.message}`)
    } finally {
      setLoading(false)
    }
  }

  const rotate = async () => {
    if (!workflowId) return
    const resp = await fetch(`/api/workflows/${workflowId}/trigger/rotate`, { method: 'POST' })
    if (resp.ok) {
      setCfg(await resp.json())
      setMsg('已重新生成 token，旧链接立即失效')
    }
  }

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setMsg('已复制到剪贴板')
    } catch {
      setMsg(text)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[480px] h-full fe-drawer fe-scroll-thin"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="fe-drawer__hd">
          <div className="fe-logo-mark" style={{ width: 24, height: 24, borderRadius: 7 }}>
            <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
              <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="white" />
            </svg>
          </div>
          <div className="flex-1">
            <h2 className="text-sm font-semibold text-white">触发器</h2>
            <p className="text-[10px] text-gray-500">
              让工作流被别人调用，或按计划自动运行
            </p>
          </div>
          <button onClick={onClose} className="fe-drawer__x">
            ×
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-5 py-4 flex flex-col gap-4">
          {!workflowId && (
            <div className="px-3 py-2 rounded-lg bg-warning/10 border border-warning/30 text-[11px] text-amber-300">
              先保存工作流，才能配置触发器
            </div>
          )}

          {/* Webhook */}
          <div className="rounded-xl border border-border bg-surface/60 px-3 py-3">
            <div className="flex items-center gap-2 mb-2">
              <span className="text-xs font-medium text-white">Webhook</span>
              <span className="text-[10px] text-gray-500">外部系统 POST 一下即可触发</span>
            </div>

            {cfg?.webhook_path ? (
              <>
                <div className="flex items-center gap-1.5">
                  <code className="flex-1 text-[10px] font-mono text-cyan-300 bg-canvas rounded px-2 py-1.5 truncate">
                    POST {cfg.webhook_path}
                  </code>
                  <button
                    onClick={() => copy(`http://127.0.0.1:8080${cfg.webhook_path}`)}
                    className="fe-btn-ghost px-2 py-1 rounded text-[10px] shrink-0"
                  >
                    复制
                  </button>
                </div>
                {cfg.curl_example && (
                  <code className="block mt-1.5 text-[9px] font-mono text-gray-500 bg-canvas rounded px-2 py-1.5 whitespace-pre-wrap break-all">
                    {cfg.curl_example}
                  </code>
                )}
                <div className="flex items-center gap-2 mt-2">
                  <button onClick={rotate} className="fe-btn-ghost px-2 py-1 rounded text-[10px]">
                    重新生成 token
                  </button>
                  {cfg.last_triggered_at && (
                    <span className="text-[10px] text-gray-500">
                      上次触发 {new Date(cfg.last_triggered_at).toLocaleString()}
                    </span>
                  )}
                </div>
                <p className="text-[10px] text-gray-600 mt-1.5">
                  token 即凭据，请勿公开；泄露后点「重新生成」即可作废旧链接
                </p>
              </>
            ) : (
              <p className="text-[11px] text-gray-500">保存一次触发器配置后自动生成 webhook 地址</p>
            )}
          </div>

          {/* Cron */}
          <div className="rounded-xl border border-border bg-surface/60 px-3 py-3 flex flex-col gap-2.5">
            <div className="flex items-center gap-2">
              <span className="text-xs font-medium text-white">定时计划</span>
              <span className="text-[10px] text-gray-500">cron 五段式（分 时 日 月 周）</span>
            </div>

            <div className="flex flex-wrap gap-1">
              {PRESETS.map((p) => (
                <button
                  key={p.cron}
                  onClick={() => setCron(p.cron)}
                  className={`text-[10px] px-2 py-1 rounded border transition-all ${
                    cron === p.cron
                      ? 'bg-primary/20 border-primary/50 text-white'
                      : 'bg-canvas border-border text-gray-400 hover:border-primary/40'
                  }`}
                >
                  {p.label}
                </button>
              ))}
            </div>

            <input
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              placeholder="*/5 * * * *"
              className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm font-mono focus:border-primary outline-none"
            />
            {cfg?.cron_description && cron === cfg.cron && (
              <p className="text-[10px] text-success">当前生效：{cfg.cron_description}</p>
            )}
            {!cron && <p className="text-[10px] text-gray-500">留空表示只使用 Webhook 触发</p>}

            <label className="flex items-center gap-2 text-[11px] text-gray-300 cursor-pointer mt-1">
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
                className="accent-indigo-500"
              />
              启用触发器（关闭后 Webhook 与定时都不再执行）
            </label>
          </div>

          {msg && <p className="text-[11px] text-indigo-300">{msg}</p>}

          <button
            onClick={save}
            disabled={loading || !workflowId}
            className="fe-btn-primary py-2 rounded-lg text-sm font-medium disabled:opacity-40"
          >
            {loading ? '保存中…' : '保存触发器配置'}
          </button>
        </div>
      </div>
    </div>
  )
}
