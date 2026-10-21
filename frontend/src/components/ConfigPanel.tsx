import { useCallback, useEffect, useState } from 'react'
import { toast } from './Toast'

/**
 * 配置面板：模型角色 / ComfyUI 实例 / 对外接入
 *
 * 为什么要有它：这三样以前都只能手改 `data/models.json`、`data/instances.json`
 * 或自己拼 MCP 客户端配置。引擎能力齐全但用户看不见——等于不存在。
 *
 * 分工原则：
 *  - **模型角色用表单**：改权重名、调步数是高频动作，手写 JSON 容易漏逗号
 *  - **实例用表单但保留 JSON 兜底**：字段少，直接编辑更直观
 *  - **MCP 用生成 + 复制**：路径和端口只有引擎知道，让用户自己拼必然填错
 */

interface BundleRole {
  label?: string
  files?: Record<string, string>
  params?: Record<string, number>
  notes?: string
  ladder?: string[]
}

interface InstanceRec {
  name: string
  base_url: string
  models?: string[]
  exclusive_group?: string
  reserve_vram?: number
  enabled: boolean
  note?: string
}

interface McpConfig {
  mcpServers: any
  exe: string
  os: string
  openai_base_url: string
  notes: string[]
}

const TABS = [
  { key: 'models', label: '模型角色' },
  { key: 'instances', label: 'ComfyUI 实例' },
  { key: 'mcp', label: '对外接入' },
] as const

type TabKey = (typeof TABS)[number]['key']

function CopyBtn({ text, label = '复制' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
        } catch {
          // 非 https / 无权限时退回 execCommand
          const ta = document.createElement('textarea')
          ta.value = text
          document.body.appendChild(ta)
          ta.select()
          document.execCommand('copy')
          document.body.removeChild(ta)
        }
        setDone(true)
        setTimeout(() => setDone(false), 1500)
      }}
      className="px-2 py-0.5 rounded border border-border text-[10px] text-gray-300 hover:border-primary/60 hover:text-primary"
    >
      {done ? '已复制' : label}
    </button>
  )
}

export default function ConfigPanel({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState<TabKey>('models')
  const [roles, setRoles] = useState<Record<string, BundleRole>>({})
  const [bundlesPath, setBundlesPath] = useState('')
  const [instances, setInstances] = useState<InstanceRec[]>([])
  const [instPath, setInstPath] = useState('')
  const [mcp, setMcp] = useState<McpConfig | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [dirty, setDirty] = useState<Record<string, boolean>>({})

  const loadModels = useCallback(async () => {
    try {
      const resp = await fetch('/api/models/bundles')
      const d = await resp.json()
      setRoles(d.roles || {})
      setBundlesPath(d.path || '')
      if (d.load_error) setErr(String(d.load_error))
    } catch (e: any) {
      setErr(String(e?.message || e))
    }
  }, [])

  const loadInstances = useCallback(async () => {
    try {
      const resp = await fetch('/api/instances')
      const d = await resp.json()
      setInstances(d.instances || [])
      setInstPath(d.path || '')
      if (d.note) setErr(String(d.note))
    } catch (e: any) {
      setErr(String(e?.message || e))
    }
  }, [])

  const loadMcp = useCallback(async () => {
    try {
      const resp = await fetch('/api/mcp/config')
      setMcp(await resp.json())
    } catch (e: any) {
      setErr(String(e?.message || e))
    }
  }, [])

  useEffect(() => {
    loadModels()
    loadInstances()
    loadMcp()
  }, [loadModels, loadInstances, loadMcp])

  const editFile = (role: string, key: string, value: string) => {
    setRoles((prev) => ({
      ...prev,
      [role]: { ...prev[role], files: { ...(prev[role].files || {}), [key]: value } },
    }))
    setDirty((d) => ({ ...d, [role]: true }))
  }

  const editParam = (role: string, key: string, value: number) => {
    setRoles((prev) => ({
      ...prev,
      [role]: { ...prev[role], params: { ...(prev[role].params || {}), [key]: value } },
    }))
    setDirty((d) => ({ ...d, [role]: true }))
  }

  const saveRole = async (role: string) => {
    const r = roles[role]
    setBusy(role)
    try {
      const resp = await fetch('/api/models/bundles', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role, files: r.files || {}, params: r.params || {} }),
      })
      if (!resp.ok) throw new Error((await resp.text()).slice(0, 200))
      toast(`已保存 ${role}，下次出图生效`, 'success')
      setDirty((d) => ({ ...d, [role]: false }))
      // 权重可能换了，object_info 缓存后端已作废
    } catch (e: any) {
      toast('保存失败：' + String(e?.message || e), 'error')
    } finally {
      setBusy('')
    }
  }

  const saveInstances = async () => {
    setBusy('instances')
    try {
      for (const it of instances) {
        if (!it.name.trim() || !it.base_url.trim()) {
          throw new Error('每个实例都要有 name 与 base_url')
        }
      }
      const resp = await fetch('/api/instances', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instances }),
      })
      if (!resp.ok) throw new Error((await resp.text()).slice(0, 200))
      toast('实例路由已保存', 'success')
    } catch (e: any) {
      toast('保存失败：' + String(e?.message || e), 'error')
    } finally {
      setBusy('')
    }
  }

  const mcpJSON = mcp ? JSON.stringify({ mcpServers: mcp.mcpServers }, null, 2) : ''

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[560px] h-full bg-[#0D121C]/95 backdrop-blur-xl border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 py-3 border-b border-border">
          <div className="text-sm font-medium text-gray-200">配置</div>
          <button onClick={onClose} className="text-gray-500 hover:text-gray-200 text-lg leading-none">
            ×
          </button>
        </div>

        <div className="flex gap-1 px-4 pt-3">
          {TABS.map((t) => (
            <button
              key={t.key}
              onClick={() => setTab(t.key)}
              className={`px-2.5 py-1 rounded-lg text-[11px] border ${
                tab === t.key
                  ? 'bg-primary/15 border-primary/50 text-primary'
                  : 'border-border text-gray-400 hover:text-gray-200'
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>

        {err && (
          <div className="mx-4 mt-2 text-[10px] text-warning bg-warning/10 border border-warning/30 rounded-lg p-2">
            {err}
          </div>
        )}

        <div className="flex-1 overflow-y-auto px-4 py-3">
          {/* ---------- 模型角色 ---------- */}
          {tab === 'models' && (
            <div className="flex flex-col gap-3">
              <p className="text-[10px] text-gray-500 leading-relaxed">
                逻辑角色 → 实际权重文件。改这里不用重编译 exe。
                权重文件必须在 ComfyUI 的 <span className="text-gray-400">models/</span> 里已存在，
                否则出图会被提交前预检拦下并提示。
              </p>
              {Object.entries(roles).map(([role, r]) => (
                <div key={role} className="rounded-xl border border-border/70 bg-canvas/40 p-2.5">
                  <div className="flex items-center justify-between mb-1.5">
                    <div>
                      <div className="text-[11px] text-gray-200">{r.label || role}</div>
                      <div className="text-[9px] text-gray-600 font-mono">{role}</div>
                    </div>
                    <button
                      onClick={() => saveRole(role)}
                      disabled={busy === role || !dirty[role]}
                      className="px-2 py-0.5 rounded border border-border text-[10px] text-gray-300 hover:border-primary/60 hover:text-primary disabled:opacity-40"
                    >
                      {busy === role ? '保存中' : '保存'}
                    </button>
                  </div>
                  {Object.entries(r.files || {}).map(([k, v]) => (
                    <div key={k} className="flex items-center gap-2 mb-1">
                      <span className="text-[9px] text-gray-500 w-16 shrink-0 font-mono">{k}</span>
                      <input
                        value={v}
                        onChange={(e) => editFile(role, k, e.target.value)}
                        className="flex-1 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] font-mono"
                      />
                    </div>
                  ))}
                  {Object.keys(r.params || {}).length > 0 && (
                    <div className="flex flex-wrap gap-2 mt-1.5">
                      {Object.entries(r.params || {}).map(([k, v]) => (
                        <label key={k} className="flex items-center gap-1">
                          <span className="text-[9px] text-gray-500 font-mono">{k}</span>
                          <input
                            type="number"
                            value={v}
                            step="any"
                            onChange={(e) => editParam(role, k, Number(e.target.value) || 0)}
                            className="w-16 px-1 py-0.5 rounded bg-canvas border border-border text-[9px] font-mono"
                          />
                        </label>
                      ))}
                    </div>
                  )}
                  {r.notes && <div className="mt-1.5 text-[9px] text-gray-600 leading-snug">{r.notes}</div>}
                </div>
              ))}
              {bundlesPath && (
                <div className="text-[9px] text-gray-600 font-mono break-all">配置文件：{bundlesPath}</div>
              )}
            </div>
          )}

          {/* ---------- 实例 ---------- */}
          {tab === 'instances' && (
            <div className="flex flex-col gap-3">
              <p className="text-[10px] text-gray-500 leading-relaxed">
                按模型名把请求路由到不同 ComfyUI 实例。同一
                <span className="text-gray-400"> 互斥组 </span>
                的实例不会同时驻留——这是单卡上跑两套环境的唯一安全做法。
              </p>
              {instances.map((it, i) => (
                <div key={i} className="rounded-xl border border-border/70 bg-canvas/40 p-2.5">
                  <div className="flex items-center gap-2 mb-1.5">
                    <input
                      value={it.name}
                      onChange={(e) => {
                        const next = [...instances]
                        next[i] = { ...it, name: e.target.value }
                        setInstances(next)
                      }}
                      className="w-24 px-1.5 py-0.5 rounded bg-canvas border border-border text-[11px]"
                    />
                    <input
                      value={it.base_url}
                      onChange={(e) => {
                        const next = [...instances]
                        next[i] = { ...it, base_url: e.target.value }
                        setInstances(next)
                      }}
                      className="flex-1 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] font-mono"
                    />
                    <label className="flex items-center gap-1 text-[9px] text-gray-400">
                      <input
                        type="checkbox"
                        checked={it.enabled}
                        onChange={(e) => {
                          const next = [...instances]
                          next[i] = { ...it, enabled: e.target.checked }
                          setInstances(next)
                        }}
                      />
                      启用
                    </label>
                  </div>
                  <div className="flex items-center gap-2">
                    <span className="text-[9px] text-gray-500 w-16 shrink-0">模型通配</span>
                    <input
                      value={(it.models || []).join(', ')}
                      onChange={(e) => {
                        const next = [...instances]
                        next[i] = {
                          ...it,
                          models: e.target.value
                            .split(',')
                            .map((s) => s.trim())
                            .filter(Boolean),
                        }
                        setInstances(next)
                      }}
                      placeholder="minimax_h3*, wan*"
                      className="flex-1 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] font-mono"
                    />
                    <input
                      value={it.exclusive_group || ''}
                      onChange={(e) => {
                        const next = [...instances]
                        next[i] = { ...it, exclusive_group: e.target.value }
                        setInstances(next)
                      }}
                      placeholder="互斥组"
                      className="w-20 px-1.5 py-0.5 rounded bg-canvas border border-border text-[9px]"
                    />
                  </div>
                  {it.note && <div className="mt-1 text-[9px] text-gray-600">{it.note}</div>}
                </div>
              ))}
              <div className="flex items-center gap-2">
                <button
                  onClick={() =>
                    setInstances([
                      ...instances,
                      { name: 'new', base_url: 'http://127.0.0.1:8188', models: [], enabled: true },
                    ])
                  }
                  className="px-2 py-0.5 rounded border border-border text-[10px] text-gray-300 hover:border-primary/60"
                >
                  + 添加实例
                </button>
                <button
                  onClick={saveInstances}
                  disabled={busy === 'instances'}
                  className="px-2 py-0.5 rounded bg-primary/15 border border-primary/50 text-[10px] text-primary hover:bg-primary/25 disabled:opacity-40"
                >
                  {busy === 'instances' ? '保存中' : '保存路由'}
                </button>
              </div>
              {instPath && <div className="text-[9px] text-gray-600 font-mono break-all">配置文件：{instPath}</div>}
            </div>
          )}

          {/* ---------- 对外接入 ---------- */}
          {tab === 'mcp' && (
            <div className="flex flex-col gap-3">
              <div>
                <div className="flex items-center justify-between mb-1">
                  <span className="text-[11px] text-gray-200">MCP 客户端配置</span>
                  <CopyBtn text={mcpJSON} />
                </div>
                <p className="text-[10px] text-gray-500 mb-1.5 leading-relaxed">
                  Claude Desktop / Cursor 加上这段，就能直接调用 flowedge_run_workflow 等工具。
                </p>
                <pre className="text-[9px] font-mono text-gray-300 bg-canvas border border-border rounded-lg p-2 overflow-x-auto whitespace-pre">
                  {mcpJSON || '加载中…'}
                </pre>
              </div>

              <div>
                <div className="flex items-center justify-between mb-1">
                  <span className="text-[11px] text-gray-200">OpenAI 兼容 Base URL</span>
                  <CopyBtn text={mcp?.openai_base_url || ''} />
                </div>
                <code className="block text-[10px] font-mono text-primary bg-canvas border border-border rounded-lg p-2">
                  {mcp?.openai_base_url || '—'}
                </code>
                <p className="text-[9px] text-gray-600 mt-1 leading-relaxed">
                  Open WebUI / SillyTavern / Chatbox 填这个地址，就能用本地 GPU 对话、出图、转写、配音。
                </p>
              </div>

              <div className="text-[9px] text-gray-500 leading-relaxed">
                {mcp?.notes?.map((n, i) => <div key={i}>· {n}</div>)}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
