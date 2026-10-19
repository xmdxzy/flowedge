import { useCallback, useEffect, useState } from 'react'

interface ModelFile {
  name: string
  size_mb: number
  path: string
}

interface ModelFamily {
  key: string
  label: string
  root: string
  count: number
  size_mb: number
  files: ModelFile[]
  note?: string
}

interface Inventory {
  root: string
  total_gb: number
  families: ModelFamily[]
  missing?: string[]
}

// 模型资产：一眼看清「有哪些模型、多大、在哪」
// 权重统一放 D 盘模型根，项目目录只存配置——这里把这条约定显式呈现出来。
export default function ModelsSection() {
  const [inv, setInv] = useState<Inventory | null>(null)
  const [ollama, setOllama] = useState<string[]>([])
  const [vision, setVision] = useState('')
  const [openKey, setOpenKey] = useState('')
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      const resp = await fetch('/api/models')
      const d = await resp.json()
      setInv(d.inventory)
      setOllama(Array.isArray(d.ollama_models) ? d.ollama_models : [])
      setVision(d.vision_model || '')
    } catch (e: any) {
      setError(String(e?.message || e))
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  if (error) return <p className="text-[10px] text-error">{error}</p>
  if (!inv) return <p className="text-[10px] text-gray-500">清点中…</p>

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center gap-2 px-2.5 py-1.5 rounded-lg bg-canvas border border-border">
        <span className="text-[10px] text-gray-500">模型根</span>
        <span className="text-[10px] font-mono text-gray-300 truncate flex-1" title={inv.root}>
          {inv.root}
        </span>
        <span className="text-[10px] text-primary font-mono">{inv.total_gb.toFixed(1)} GB</span>
      </div>

      {inv.families.map((fam) => {
        const open = openKey === fam.key
        const missing = fam.count === 0
        return (
          <div key={fam.key} className={`rounded-lg border px-2.5 py-1.5 ${missing ? 'border-border/60 bg-surface/40' : 'border-border bg-canvas'}`}>
            <button
              onClick={() => setOpenKey(open ? '' : fam.key)}
              className="w-full flex items-center gap-2 text-left"
            >
              <span className={`text-[10px] ${missing ? 'text-gray-600' : 'text-success'}`}>{missing ? '○' : '●'}</span>
              <span className="text-[11px] text-white truncate">{fam.label}</span>
              <span className="ml-auto text-[10px] text-gray-500 font-mono shrink-0">
                {fam.count} 个 · {(fam.size_mb / 1024).toFixed(1)} GB
              </span>
            </button>
            {fam.note && <p className="text-[9px] text-gray-600 mt-0.5">{fam.note}</p>}
            {open && fam.files.length > 0 && (
              <div className="mt-1.5 flex flex-col gap-0.5 border-t border-border/60 pt-1.5">
                {fam.files.map((f) => (
                  <div key={f.path} className="flex items-center gap-2">
                    <span className="text-[9px] text-gray-400 font-mono truncate flex-1" title={f.path}>
                      {f.name}
                    </span>
                    <span className="text-[9px] text-gray-600 font-mono shrink-0">{(f.size_mb / 1024).toFixed(1)}G</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )
      })}

      <div className="mt-1 px-2.5 py-1.5 rounded-lg bg-canvas border border-border">
        <p className="text-[10px] text-gray-500 mb-1">Ollama 已装模型 {ollama.length > 0 ? `(${ollama.length})` : ''}</p>
        <div className="flex flex-wrap gap-1">
          {ollama.length === 0 && <span className="text-[9px] text-gray-600">无（先 ollama pull 一个模型）</span>}
          {ollama.map((m) => (
            <span
              key={m}
              className={`text-[9px] px-1.5 py-0.5 rounded font-mono ${
                m === vision ? 'bg-teal-400/15 text-teal-300' : 'bg-ai/15 text-ai'
              }`}
              title={m === vision ? '当前视觉模型' : undefined}
            >
              {m}
            </span>
          ))}
        </div>
      </div>
    </div>
  )
}
