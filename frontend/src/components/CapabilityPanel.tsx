import { useCallback, useEffect, useState } from 'react'

interface Capability {
  ok: boolean
  detail?: string
  hint?: string
  models?: string[]
  endpoint?: string
  path?: string
}

interface Capabilities {
  os: string
  base_dir: string
  gpu: Capability
  ollama: Capability
  comfyui: Capability
  ffmpeg: Capability
  asr: Capability
  tts: Capability
  ready: Record<string, boolean>
  missing?: string[]
}

interface Props {
  onClose: () => void
}

const ITEMS: { key: keyof Capabilities; label: string; need: string; effect: string }[] = [
  { key: 'gpu', label: 'NVIDIA GPU', need: '显卡驱动', effect: 'CUDA 推理' },
  { key: 'ollama', label: 'Ollama', need: 'ollama.com 安装包', effect: 'LLM 推理 / 知识库向量化' },
  { key: 'comfyui', label: 'ComfyUI', need: '本地 ComfyUI', effect: '文生图' },
  { key: 'ffmpeg', label: 'ffmpeg', need: 'gyan.dev 静态包', effect: '视频合成' },
  { key: 'asr', label: '语音转写服务', need: 'Python + faster-whisper', effect: '语音转文字' },
  { key: 'tts', label: '语音合成服务', need: 'Python（Windows 自带 SAPI）', effect: '文字转语音' },
]

export default function CapabilityPanel({ onClose }: Props) {
  const [caps, setCaps] = useState<Capabilities | null>(null)
  const [meta, setMeta] = useState<{ version?: string; uptime_seconds?: number }>({})
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const refresh = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const resp = await fetch('/api/capabilities')
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || 'probe failed')
      setCaps(data.capabilities)
      setMeta({ version: data.version, uptime_seconds: data.uptime_seconds })
    } catch (err: any) {
      setError(`检测失败：${err.message}`)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  const okCount = caps ? ITEMS.filter((i) => (caps[i.key] as Capability)?.ok).length : 0
  const uptime = meta.uptime_seconds ? Math.floor(meta.uptime_seconds / 60) : 0

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[480px] h-full bg-[#0D121C]/95 backdrop-blur-xl border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="flex items-center gap-3 px-5 py-4 border-b border-border">
          <div className="fe-logo-mark" style={{ width: 24, height: 24, borderRadius: 7 }}>
            <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
              <path d="M8 1L2 8h4l-1 7 7-9H8l1-5z" fill="white" />
            </svg>
          </div>
          <div className="flex-1">
            <h2 className="text-sm font-semibold text-white">运行环境</h2>
            <p className="text-[10px] text-gray-500">
              {caps ? `${okCount}/${ITEMS.length} 项就绪` : '检测中…'}
              {meta.version ? ` · v${meta.version}` : ''}
              {uptime > 0 ? ` · 已运行 ${uptime} 分钟` : ''}
            </p>
          </div>
          <button
            onClick={refresh}
            disabled={loading}
            className="fe-btn-ghost px-2.5 py-1 rounded-lg text-[11px] disabled:opacity-50"
          >
            {loading ? '检测中…' : '重新检测'}
          </button>
          <button onClick={onClose} className="text-gray-500 hover:text-white text-lg leading-none px-1">
            ×
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-5 py-4">
          {error && (
            <div className="mb-3 px-3 py-2 rounded-lg bg-error/10 border border-error/30 text-[11px] text-red-300">
              {error}
            </div>
          )}

          <div className="flex flex-col gap-2">
            {ITEMS.map((item) => {
              const cap = (caps?.[item.key] as Capability) || null
              const ok = !!cap?.ok
              return (
                <div
                  key={item.key}
                  className={`rounded-xl border px-3 py-2.5 ${
                    ok ? 'border-success/25 bg-success/5' : 'border-border bg-surface/60'
                  }`}
                >
                  <div className="flex items-center gap-2">
                    <span className={`text-[11px] ${ok ? 'text-success' : 'text-gray-600'}`}>
                      {ok ? '●' : '○'}
                    </span>
                    <span className="text-xs font-medium text-white">{item.label}</span>
                    <span className="text-[10px] text-gray-500">{item.effect}</span>
                    <span
                      className={`ml-auto text-[10px] px-1.5 py-0.5 rounded ${
                        ok ? 'bg-success/15 text-success' : 'bg-surface-hover text-gray-500'
                      }`}
                    >
                      {ok ? '可用' : '未启用'}
                    </span>
                  </div>

                  {cap?.detail && (
                    <p className="text-[10px] text-gray-500 mt-1 font-mono truncate">{cap.detail}</p>
                  )}

                  {!ok && (
                    <p className="text-[10px] text-gray-400 mt-1.5 leading-relaxed">
                      <span className="text-warning">怎么补：</span>
                      {cap?.hint || `需要 ${item.need}`}
                    </p>
                  )}

                  {item.key === 'ollama' && (cap?.models?.length ?? 0) > 0 && (
                    <div className="flex flex-wrap gap-1 mt-1.5">
                      {cap!.models!.slice(0, 6).map((m) => (
                        <span
                          key={m}
                          className="text-[9px] px-1.5 py-0.5 rounded bg-ai/15 text-ai font-mono"
                        >
                          {m}
                        </span>
                      ))}
                      {cap!.models!.length > 6 && (
                        <span className="text-[9px] text-gray-500">+{cap!.models!.length - 6}</span>
                      )}
                    </div>
                  )}
                </div>
              )
            })}
          </div>

          <div className="mt-4 pt-3 border-t border-border">
            <p className="text-[10px] text-gray-500 leading-relaxed">
              缺失的能力不影响其他功能：没有 Ollama 仍可编排流程、管理知识库；没有 ComfyUI 仍可用
              LLM 与语音节点。数据目录：
              <span className="text-gray-400 font-mono"> {caps?.base_dir || '—'}\data</span>
            </p>
            <p className="text-[10px] text-gray-500 mt-1.5">
              系统：<span className="font-mono text-gray-400">{caps?.os || '—'}</span>
              {' · '}无需 Docker / WSL
            </p>
          </div>
        </div>
      </div>
    </div>
  )
}
