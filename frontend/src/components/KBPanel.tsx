import { useEffect, useState } from 'react'

interface KBDoc {
  id: string
  source: string
  content: string
}

interface KBResult {
  id: string
  source: string
  content: string
  score: number
}

interface KBPanelProps {
  open: boolean
  onClose: () => void
}

type Tab = 'list' | 'add' | 'import' | 'query'

export default function KBPanel({ open, onClose }: KBPanelProps) {
  const [tab, setTab] = useState<Tab>('list')
  const [status, setStatus] = useState<{ embedder?: string; chunks?: number }>({})
  const [docs, setDocs] = useState<KBDoc[]>([])
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState('')

  const [newSource, setNewSource] = useState('')
  const [newContent, setNewContent] = useState('')
  const [query, setQuery] = useState('')
  const [topK, setTopK] = useState(3)
  const [results, setResults] = useState<KBResult[]>([])

  const refresh = async () => {
    try {
      const [s, d] = await Promise.all([
        fetch('/api/kb/status').then((r) => r.json()),
        fetch('/api/kb/documents').then((r) => r.json()),
      ])
      setStatus(s)
      setDocs(d.documents || [])
    } catch (err) {
      console.error('kb refresh failed', err)
    }
  }

  useEffect(() => {
    if (open) refresh()
  }, [open])

  if (!open) return null

  const addDocument = async () => {
    if (!newContent.trim()) {
      setMessage('内容不能为空')
      return
    }
    setLoading(true)
    setMessage('')
    try {
      const resp = await fetch('/api/kb/documents', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: newContent, source: newSource || '手动录入' }),
      })
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || 'add failed')
      setMessage(`已入库 ${data.chunks} 个知识块（总计 ${data.total}）`)
      setNewContent('')
      setNewSource('')
      refresh()
    } catch (err: any) {
      setMessage(`失败: ${err.message}`)
    } finally {
      setLoading(false)
    }
  }

  const importFile = async (file: File) => {
    setLoading(true)
    setMessage('')
    try {
      const form = new FormData()
      form.append('file', file)
      const resp = await fetch('/api/kb/import', { method: 'POST', body: form })
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || 'import failed')
      setMessage(`已导入「${data.file}」${data.chunks} 个知识块（总计 ${data.total}）`)
      refresh()
    } catch (err: any) {
      setMessage(`失败: ${err.message}`)
    } finally {
      setLoading(false)
    }
  }

  const runQuery = async () => {
    if (!query.trim()) return
    setLoading(true)
    setMessage('')
    try {
      const resp = await fetch('/api/kb/query', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query, top_k: topK }),
      })
      const data = await resp.json()
      if (!resp.ok) throw new Error(data.error || 'query failed')
      setResults(data.results || [])
    } catch (err: any) {
      setMessage(`失败: ${err.message}`)
    } finally {
      setLoading(false)
    }
  }

  const removeDoc = async (id: string) => {
    await fetch(`/api/kb/documents/${id}`, { method: 'DELETE' })
    refresh()
  }

  const tabBtn = (key: Tab, label: string) => (
    <button
      onClick={() => { setTab(key); setMessage('') }}
      className={`px-3 py-1.5 rounded-lg text-xs font-medium transition-all ${
        tab === key
          ? 'bg-primary/20 text-indigo-300 border border-primary/40'
          : 'text-gray-400 hover:text-white border border-transparent'
      }`}
    >
      {label}
    </button>
  )

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[520px] h-full bg-[#0D121C]/95 backdrop-blur-xl border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="flex items-center gap-3 px-5 py-4 border-b border-border">
          <div className="fe-logo-mark" style={{ width: 24, height: 24, borderRadius: 7 }}>
            <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
              <path d="M8 1L2 8h4l-1 7 7-9H8l1-5z" fill="white" />
            </svg>
          </div>
          <div>
            <h2 className="text-sm font-semibold text-white">项目知识库</h2>
            <p className="text-[10px] text-gray-500">
              {status.embedder === 'ollama' ? 'bge-m3 语义向量' : '词面向量（降级）'} · {status.chunks ?? 0} 个知识块
            </p>
          </div>
          <button onClick={onClose} className="ml-auto text-gray-500 hover:text-white text-lg leading-none px-2">
            ×
          </button>
        </div>

        {/* Tab 栏 */}
        <div className="flex gap-1 px-4 py-2 border-b border-border">
          {tabBtn('list', '知识块')}
          {tabBtn('add', '添加')}
          {tabBtn('import', '导入文件')}
          {tabBtn('query', '检索测试')}
        </div>

        {message && (
          <div className="mx-4 mt-3 px-3 py-2 rounded-lg bg-primary/10 border border-primary/30 text-xs text-indigo-300">
            {message}
          </div>
        )}

        {/* 内容区 */}
        <div className="flex-1 overflow-y-auto p-4">
          {tab === 'list' && (
            <div className="flex flex-col gap-2">
              {docs.length === 0 && <p className="text-xs text-gray-500">知识库为空</p>}
              {docs.map((d) => (
                <div key={d.id} className="px-3 py-2.5 rounded-lg bg-surface border border-border">
                  <div className="flex items-center gap-2 mb-1">
                    <span className="text-[10px] px-1.5 py-0.5 rounded bg-accent/10 text-cyan-300">{d.source || '未知来源'}</span>
                    <button
                      onClick={() => removeDoc(d.id)}
                      className="ml-auto text-[10px] text-gray-500 hover:text-error"
                    >
                      删除
                    </button>
                  </div>
                  <p className="text-xs text-gray-300 leading-relaxed line-clamp-3">{d.content}</p>
                </div>
              ))}
            </div>
          )}

          {tab === 'add' && (
            <div className="flex flex-col gap-3">
              <input
                value={newSource}
                onChange={(e) => setNewSource(e.target.value)}
                placeholder="来源标签（可选，如：运维手册）"
                className="px-3 py-2 rounded-lg bg-canvas border border-border text-sm"
              />
              <textarea
                value={newContent}
                onChange={(e) => setNewContent(e.target.value)}
                placeholder="粘贴文档内容，段落之间用空行分隔（自动分块）"
                className="h-64 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none"
              />
              <button
                onClick={addDocument}
                disabled={loading}
                className="fe-btn-primary px-4 py-2 rounded-lg text-sm font-medium"
              >
                {loading ? '入库中...' : '写入知识库'}
              </button>
            </div>
          )}

          {tab === 'import' && (
            <div className="flex flex-col gap-3">
              <label className="flex flex-col items-center justify-center h-40 rounded-xl border border-dashed border-border bg-canvas cursor-pointer hover:border-primary/60 transition-all">
                <p className="text-sm text-gray-300">点击选择 .md / .txt 文件</p>
                <p className="text-[10px] text-gray-500 mt-1">上限 2MB，自动段落分块并向量化</p>
                <input
                  type="file"
                  accept=".md,.txt"
                  className="hidden"
                  onChange={(e) => {
                    const f = e.target.files?.[0]
                    if (f) importFile(f)
                    e.target.value = ''
                  }}
                />
              </label>
              {loading && <p className="text-xs text-indigo-300">向量化中（bge-m3）...</p>}
            </div>
          )}

          {tab === 'query' && (
            <div className="flex flex-col gap-3">
              <div className="flex gap-2">
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && runQuery()}
                  placeholder="输入与文档措辞不同的问法，验证语义检索..."
                  className="flex-1 px-3 py-2 rounded-lg bg-canvas border border-border text-sm"
                />
                <select
                  value={topK}
                  onChange={(e) => setTopK(Number(e.target.value))}
                  className="px-2 py-2 rounded-lg bg-canvas border border-border text-sm"
                >
                  <option value={1}>1</option>
                  <option value={3}>3</option>
                  <option value={5}>5</option>
                </select>
                <button onClick={runQuery} disabled={loading} className="fe-btn-primary px-4 rounded-lg text-sm">
                  检索
                </button>
              </div>
              {results.map((r, i) => (
                <div key={r.id} className="px-3 py-2.5 rounded-lg bg-surface border border-border">
                  <div className="flex items-center gap-2 mb-1">
                    <span className="text-[10px] text-gray-500">#{i + 1}</span>
                    <span className="text-[10px] px-1.5 py-0.5 rounded bg-accent/10 text-cyan-300">{r.source}</span>
                    <div className="ml-auto flex items-center gap-2">
                      <div className="w-16 h-1.5 rounded-full bg-border overflow-hidden">
                        <div className="h-full bg-gradient-to-r from-primary to-accent" style={{ width: `${Math.min(r.score, 1) * 100}%` }} />
                      </div>
                      <span className="text-[10px] font-mono text-cyan-300">{r.score.toFixed(3)}</span>
                    </div>
                  </div>
                  <p className="text-xs text-gray-300 leading-relaxed">{r.content}</p>
                </div>
              ))}
              {results.length === 0 && !loading && <p className="text-xs text-gray-500">输入问题后回车检索</p>}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
