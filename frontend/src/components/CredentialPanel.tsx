import { useCallback, useEffect, useState } from 'react'

interface MaskedCredential {
  name: string
  note?: string
  header_keys: string[]
  query_keys: string[]
  updated_at: string
}

interface Props {
  onClose: () => void
}

interface KV {
  k: string
  v: string
}

// 凭据面板：值只在提交时上行，读列表永远拿不到明文
export default function CredentialPanel({ onClose }: Props) {
  const [items, setItems] = useState<MaskedCredential[]>([])
  const [loading, setLoading] = useState(false)
  const [name, setName] = useState('')
  const [note, setNote] = useState('')
  const [headers, setHeaders] = useState<KV[]>([{ k: 'Authorization', v: 'Bearer ' }])
  const [query, setQuery] = useState<KV[]>([])
  const [error, setError] = useState('')
  const [saved, setSaved] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const resp = await fetch('/api/credentials')
      const data = await resp.json()
      setItems(data.items || [])
    } catch (e: any) {
      setError(String(e?.message || e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const save = async () => {
    setError('')
    setSaved('')
    if (!name.trim()) {
      setError('先给凭据起个名字（HTTP 节点靠它引用）')
      return
    }
    const toMap = (rows: KV[]) => {
      const out: Record<string, string> = {}
      rows.forEach((r) => {
        if (r.k.trim()) out[r.k.trim()] = r.v
      })
      return out
    }
    try {
      const resp = await fetch('/api/credentials', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: name.trim(), headers: toMap(headers), query: toMap(query), note }),
      })
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || 'save failed')
      setSaved(`已保存 ${name.trim()}（值已 AES-256-GCM 加密落盘）`)
      setName('')
      setNote('')
      load()
    } catch (e: any) {
      setError(String(e?.message || e))
    }
  }

  const remove = async (n: string) => {
    if (!confirm(`删除凭据 ${n}？引用它的 HTTP 节点会开始报错。`)) return
    await fetch(`/api/credentials/${encodeURIComponent(n)}`, { method: 'DELETE' })
    load()
  }

  const rowEditor = (rows: KV[], setRows: (v: KV[]) => void, placeholder: string) => (
    <div className="flex flex-col gap-1.5">
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-1.5">
          <input
            value={row.k}
            onChange={(e) => setRows(rows.map((r, idx) => (idx === i ? { ...r, k: e.target.value } : r)))}
            placeholder={placeholder}
            className="w-[42%] px-2 py-1 rounded bg-canvas border border-border text-[11px] font-mono outline-none"
          />
          <input
            value={row.v}
            onChange={(e) => setRows(rows.map((r, idx) => (idx === i ? { ...r, v: e.target.value } : r)))}
            placeholder="值（保存后不可再读出）"
            className="flex-1 px-2 py-1 rounded bg-canvas border border-border text-[11px] font-mono outline-none"
          />
          <button
            onClick={() => setRows(rows.filter((_, idx) => idx !== i))}
            className="text-gray-500 hover:text-error px-1 text-xs"
            title="删除这一行"
          >
            ×
          </button>
        </div>
      ))}
      <button
        onClick={() => setRows([...rows, { k: '', v: '' }])}
        className="self-start text-[10px] text-primary hover:underline"
      >
        + 加一行
      </button>
    </div>
  )

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[520px] h-full bg-[#0D121C]/95 backdrop-blur-xl border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-3 px-5 py-4 border-b border-border">
          <div className="flex-1">
            <h2 className="text-sm font-semibold text-white">凭据</h2>
            <p className="text-[10px] text-gray-500">
              API Key 不再写进节点 JSON；HTTP 节点填凭据名即可注入 header / query
            </p>
          </div>
          <button onClick={load} disabled={loading} className="fe-btn-ghost px-2.5 py-1 rounded-lg text-[11px]">
            {loading ? '刷新中…' : '刷新'}
          </button>
          <button onClick={onClose} className="fe-btn-ghost px-2.5 py-1 rounded-lg text-[11px]">
            关闭
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-5 py-4 flex flex-col gap-5">
          {/* 已有凭据 */}
          <div>
            <div className="fe-section-label mb-2">已保存 {items.length > 0 ? `(${items.length})` : ''}</div>
            {items.length === 0 ? (
              <p className="text-[11px] text-gray-500">还没有凭据。下面新建一条。</p>
            ) : (
              <div className="flex flex-col gap-1.5">
                {items.map((item) => (
                  <div
                    key={item.name}
                    className="flex items-center gap-2 px-3 py-2 rounded-lg bg-canvas border border-border"
                  >
                    <div className="min-w-0 flex-1">
                      <p className="text-xs text-white font-mono truncate">{item.name}</p>
                      <p className="text-[10px] text-gray-500 truncate">
                        {item.header_keys.map((k) => `header:${k}`).concat(item.query_keys.map((k) => `query:${k}`)).join('  ')}
                        {item.note ? `  ·  ${item.note}` : ''}
                      </p>
                    </div>
                    <button
                      onClick={() => remove(item.name)}
                      className="text-[10px] text-gray-500 hover:text-error"
                    >
                      删除
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* 新建 */}
          <div>
            <div className="fe-section-label mb-2">新建 / 覆盖</div>
            <div className="flex flex-col gap-2">
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="凭据名，如 openai / github"
                className="px-2.5 py-1.5 rounded-lg bg-canvas border border-border text-xs outline-none"
              />
              <label className="text-[10px] text-gray-500">Headers</label>
              {rowEditor(headers, setHeaders, 'Header 名')}
              <label className="text-[10px] text-gray-500 mt-1">Query 参数（可选）</label>
              {rowEditor(query, setQuery, '参数名')}
              <input
                value={note}
                onChange={(e) => setNote(e.target.value)}
                placeholder="备注（可选）"
                className="px-2.5 py-1.5 rounded-lg bg-canvas border border-border text-xs outline-none"
              />
              <button onClick={save} className="fe-btn-primary px-3 py-1.5 rounded-lg text-xs mt-1">
                保存凭据
              </button>
              {error && <p className="text-[11px] text-error">{error}</p>}
              {saved && <p className="text-[11px] text-success">{saved}</p>}
            </div>
          </div>

          <p className="text-[10px] text-gray-600 leading-relaxed">
            存储：data/credentials.json（AES-256-GCM 加密，主密钥在 data/.master_key 或 FLOWEDGE_MASTER_KEY）。
            列表接口只回名称与 header 名，值不可再读出——忘记就重新录一遍。
          </p>
        </div>
      </div>
    </div>
  )
}
