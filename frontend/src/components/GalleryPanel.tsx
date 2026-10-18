import { useCallback, useEffect, useState } from 'react'

interface Artifact {
  name: string
  kind: 'image' | 'video' | 'audio'
  size: number
  mod_time: number
}

interface Props {
  onClose: () => void
}

function fmtSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

const KIND_ICON: Record<string, string> = { image: '🖼', video: '🎬', audio: '🔊' }

export default function GalleryPanel({ onClose }: Props) {
  const [items, setItems] = useState<Artifact[]>([])
  const [dir, setDir] = useState('')
  const [preview, setPreview] = useState<Artifact | null>(null)
  const [loading, setLoading] = useState(false)
  const [filter, setFilter] = useState<'all' | 'image' | 'video' | 'audio'>('all')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const resp = await fetch('/api/artifacts')
      const data = await resp.json()
      setItems(data.items || [])
      setDir(data.dir || '')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const remove = async (name: string) => {
    if (!confirm(`删除产物 ${name}？`)) return
    await fetch(`/api/artifacts/${encodeURIComponent(name)}`, { method: 'DELETE' })
    load()
  }

  const copyPath = async (name: string) => {
    const path = `${dir}\\${name}`
    try {
      await navigator.clipboard.writeText(path)
    } catch {
      alert(path)
    }
  }

  const shown = filter === 'all' ? items : items.filter((i) => i.kind === filter)

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[640px] h-full bg-[#0D121C]/95 backdrop-blur-xl border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-3 px-5 py-4 border-b border-border">
          <div className="fe-logo-mark" style={{ width: 24, height: 24, borderRadius: 7 }}>
            <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
              <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="white" />
            </svg>
          </div>
          <div className="flex-1 min-w-0">
            <h2 className="text-sm font-semibold text-white">产物画廊</h2>
            <p className="text-[10px] text-gray-500 truncate">{dir || 'data/generated'}</p>
          </div>
          <button onClick={load} disabled={loading} className="fe-btn-ghost px-2.5 py-1 rounded-lg text-[11px]">
            {loading ? '刷新中…' : '刷新'}
          </button>
          <button onClick={onClose} className="text-gray-500 hover:text-white text-lg leading-none px-1">
            ×
          </button>
        </div>

        {/* 过滤 */}
        <div className="flex items-center gap-1.5 px-5 py-2 border-b border-border/60">
          {(['all', 'image', 'video', 'audio'] as const).map((k) => (
            <button
              key={k}
              onClick={() => setFilter(k)}
              className={`text-[11px] px-2.5 py-1 rounded-lg border transition-all ${
                filter === k
                  ? 'bg-primary/20 border-primary/40 text-white'
                  : 'bg-canvas border-border text-gray-400 hover:border-primary/40'
              }`}
            >
              {k === 'all' ? `全部 ${items.length}` : `${KIND_ICON[k]} ${items.filter((i) => i.kind === k).length}`}
            </button>
          ))}
        </div>

        <div className="flex-1 overflow-y-auto px-5 py-4">
          {shown.length === 0 ? (
            <div className="flex flex-col items-center justify-center h-48 text-center">
              <p className="text-3xl mb-2 opacity-40">🖼</p>
              <p className="text-xs text-gray-400">还没有产物</p>
              <p className="text-[10px] text-gray-600 mt-1">
                运行一次带「文生图 / 视频合成 / 语音合成」的工作流，结果会出现在这里
              </p>
            </div>
          ) : (
            <div className="grid grid-cols-3 gap-2.5">
              {shown.map((item) => (
                <div
                  key={item.name}
                  className="group relative rounded-lg overflow-hidden border border-border bg-canvas cursor-pointer hover:border-primary/60 transition-all"
                  onClick={() => setPreview(item)}
                >
                  {item.kind === 'image' ? (
                    <img
                      src={`/api/artifacts/${encodeURIComponent(item.name)}`}
                      alt={item.name}
                      className="w-full h-28 object-cover"
                      loading="lazy"
                    />
                  ) : (
                    <div className="w-full h-28 flex items-center justify-center text-2xl bg-surface">
                      {KIND_ICON[item.kind]}
                    </div>
                  )}
                  <div className="px-1.5 py-1 bg-surface/90">
                    <p className="text-[9px] text-gray-400 truncate">{item.name}</p>
                    <p className="text-[9px] text-gray-600">{fmtSize(item.size)}</p>
                  </div>
                  <button
                    onClick={(e) => {
                      e.stopPropagation()
                      remove(item.name)
                    }}
                    className="absolute top-1 right-1 opacity-0 group-hover:opacity-100 w-5 h-5 rounded bg-black/70 text-red-300 text-[10px] leading-none transition-opacity"
                    title="删除"
                  >
                    ×
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* 预览层 */}
      {preview && (
        <div
          className="fixed inset-0 z-60 bg-black/80 backdrop-blur-sm flex flex-col items-center justify-center p-8"
          onClick={(e) => {
            e.stopPropagation()
            setPreview(null)
          }}
        >
          <div className="max-w-[80vw] max-h-[75vh]">
            {preview.kind === 'image' && (
              <img
                src={`/api/artifacts/${encodeURIComponent(preview.name)}`}
                alt={preview.name}
                className="max-w-full max-h-[75vh] rounded-lg shadow-2xl"
              />
            )}
            {preview.kind === 'video' && (
              <video
                src={`/api/artifacts/${encodeURIComponent(preview.name)}`}
                controls
                autoPlay
                className="max-w-full max-h-[75vh] rounded-lg shadow-2xl"
              />
            )}
            {preview.kind === 'audio' && (
              <audio src={`/api/artifacts/${encodeURIComponent(preview.name)}`} controls autoPlay />
            )}
          </div>
          <div className="mt-3 flex items-center gap-3">
            <span className="text-xs text-gray-300 font-mono">{preview.name}</span>
            <button
              onClick={(e) => {
                e.stopPropagation()
                copyPath(preview.name)
              }}
              className="fe-btn-ghost px-2.5 py-1 rounded-lg text-[11px]"
            >
              复制路径（可在节点里复用）
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
