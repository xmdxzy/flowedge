import { useCallback, useEffect, useState } from 'react'

import { fetchProfiles, saveProfiles, type LLMProfile } from '../lib/llmProfiles'

// 自定义模型端点编辑器（配置 · 模型 里的一块）。
//
// 为什么放在"模型"而不是节点面板：节点面板只该做**选择**。
// 建端点要填 base_url / model / 凭据名，是一次性配置动作，塞进属性面板会让
// 每天点十次的表单里多出三个一辈子点一次的输入框。
//
// 密钥不在这里输入：填「凭据名」，密钥放在凭据面板（已加密落盘）。
// 这样导出工作流 JSON 时不会带出任何 key。

export default function LLMProfilesEditor() {
  const [rows, setRows] = useState<LLMProfile[]>([])
  const [def, setDef] = useState('')
  const [dirty, setDirty] = useState(false)
  const [msg, setMsg] = useState<{ text: string; bad?: boolean } | null>(null)

  const load = useCallback(async () => {
    const p = await fetchProfiles()
    if (!p) return
    setRows(p.profiles || [])
    setDef(p.default || '')
    setDirty(false)
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const patch = (i: number, k: keyof LLMProfile, v: string) => {
    setRows((prev) => prev.map((r, idx) => (idx === i ? { ...r, [k]: v } : r)))
    setDirty(true)
    setMsg(null)
  }

  const save = async () => {
    const err = await saveProfiles(rows.filter((r) => r.name.trim() || r.model.trim() || r.base_url.trim()), def)
    if (err) {
      setMsg({ text: err, bad: true })
      return
    }
    setMsg({ text: '已保存（引擎下次运行时生效）' })
    await load()
  }

  return (
    <div className="mt-2 pt-2 border-t border-border">
      <div className="flex items-center justify-between mb-1.5">
        <p className="text-[10px] text-gray-400">自定义模型（OpenAI 兼容端点）</p>
        <button
          onClick={() => {
            setRows((p) => [...p, { name: '', base_url: '', model: '' }])
            setDirty(true)
          }}
          className="text-[10px] text-primary hover:underline"
        >
          + 新增
        </button>
      </div>

      {rows.length === 0 && (
        <p className="text-[10px] text-gray-600 leading-relaxed">
          还没有自定义端点。填一条就可以在 AI 推理节点的模型下拉里选它；
          密钥请在「凭据」面板新建（headers 里放 Authorization: Bearer sk-xxx），这里只填凭据名。
        </p>
      )}

      {rows.map((r, i) => (
        <div key={i} className="mb-1.5 p-1.5 rounded-md border border-border bg-canvas/60">
          <div className="flex gap-1 mb-1">
            <input
              value={r.name}
              onChange={(e) => patch(i, 'name', e.target.value)}
              placeholder="名字（如 DeepSeek）"
              className="flex-1 min-w-0 px-1.5 py-1 rounded bg-canvas border border-border text-[10px]"
            />
            <button
              onClick={() => {
                setRows((p) => p.filter((_, idx) => idx !== i))
                setDirty(true)
              }}
              className="px-1.5 text-[10px] text-error"
              title="删除"
            >
              ×
            </button>
          </div>
          <div className="flex gap-1">
            <input
              value={r.base_url}
              onChange={(e) => patch(i, 'base_url', e.target.value)}
              placeholder="https://api.example.com/v1"
              className="flex-1 min-w-0 px-1.5 py-1 rounded bg-canvas border border-border text-[10px] font-mono"
            />
            <input
              value={r.model}
              onChange={(e) => patch(i, 'model', e.target.value)}
              placeholder="model"
              className="w-[86px] px-1.5 py-1 rounded bg-canvas border border-border text-[10px] font-mono"
            />
          </div>
          <input
            value={r.credential || ''}
            onChange={(e) => patch(i, 'credential', e.target.value)}
            placeholder="凭据名（可选，留空不带鉴权）"
            className="w-full mt-1 px-1.5 py-1 rounded bg-canvas border border-border text-[10px]"
          />
        </div>
      ))}

      {rows.length > 0 && (
        <div className="flex items-center gap-2">
          <button
            onClick={save}
            disabled={!dirty}
            className={`px-2 py-1 rounded text-[10px] ${
              dirty ? 'bg-primary/20 text-primary border border-primary/40' : 'bg-canvas text-gray-600 border border-border'
            }`}
          >
            保存模型清单
          </button>
          {def && <span className="text-[9px] text-gray-600">默认：{def}</span>}
        </div>
      )}

      {msg && (
        <p className={`text-[10px] mt-1 ${msg.bad ? 'text-error' : 'text-success'}`}>{msg.text}</p>
      )}
    </div>
  )
}
