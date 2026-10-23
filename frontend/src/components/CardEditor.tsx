import { useState } from 'react'

import type { AgentCard, CardCatalog, SkillCard } from '../lib/cards'

// 卡片管理：把"库"变成真的库——能新建、能改提示词、能删。
//
// 为什么保存是整份提交：卡片清单是一份配置，逐条增删会出现"本地改了但没提交"的中间态。
// 整份 PUT 只有一个结果状态，失败就整体不生效。
//
// 为什么 id 要手填但自动生成：id 是工作流里的引用（bindings[{id}]），
// 改名不能改 id，否则所有引用这条卡的工作流会静默失效。

interface Props {
  catalog: CardCatalog | null
  onClose: () => void
  onSaved: () => void
}

const TOOLS = [
  { id: 'kb_search', label: '知识库检索' },
  { id: 'http_get', label: 'HTTP 查询' },
  { id: 'calc', label: '计算器' },
  { id: 'now', label: '当前时间' },
]

function slug(name: string, prefix: string) {
  const s = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9\u4e00-\u9fa5]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return s ? `${prefix}-${s}` : `${prefix}-${Date.now().toString(36)}`
}

export default function CardEditor({ catalog, onClose, onSaved }: Props) {
  const [kind, setKind] = useState<'agent' | 'skill'>('agent')
  const [agents, setAgents] = useState<AgentCard[]>(catalog?.agents || [])
  const [skills, setSkills] = useState<SkillCard[]>(catalog?.skills || [])
  const [sel, setSel] = useState<string>(catalog?.agents?.[0]?.id || '')
  const [msg, setMsg] = useState<{ text: string; bad?: boolean } | null>(null)
  const [dirty, setDirty] = useState(false)

  const list = kind === 'agent' ? agents : skills
  const cur = (list as (AgentCard | SkillCard)[]).find((c) => c.id === sel)

  const patch = (p: Partial<AgentCard & SkillCard>) => {
    if (!cur) return
    setDirty(true)
    setMsg(null)
    if (kind === 'agent') {
      setAgents((prev) => prev.map((c) => (c.id === sel ? { ...c, ...p } : c)))
    } else {
      setSkills((prev) => prev.map((c) => (c.id === sel ? { ...c, ...p } : c)))
    }
  }

  const add = () => {
    const name = kind === 'agent' ? '新智能体' : '新技能'
    const id = slug(name, kind)
    if (kind === 'agent') {
      setAgents((p) => [
        ...p,
        { id, name, role: '', system: '', category: '自定义', icon: '✧', tags: [] },
      ])
    } else {
      setSkills((p) => [
        ...p,
        { id, name, desc: '', system: '', category: '自定义', icon: '⬡', out_kind: 'text', tags: [] },
      ])
    }
    setSel(id)
    setDirty(true)
  }

  // 复制当前卡：微调内置卡的正确姿势是"复制一份再改"，
  // 直接改内置卡会让下次更新时难以分辨"这是原厂设定还是我改过"。
  const dup = () => {
    if (!cur) return
    const id = `${cur.id}-copy`
    const base = { ...cur, id, name: `${cur.name} 副本` }
    if (kind === 'agent') {
      setAgents((p) => [...p, base as AgentCard])
      setKind('agent')
    } else {
      setSkills((p) => [...p, base as SkillCard])
    }
    setSel(id)
    setDirty(true)
    setMsg({ text: `已复制为 ${id}，记得改名字与提示词再保存` })
  }

  const del = () => {
    if (!cur) return
    if (!confirm(`删除「${cur.name}」？已经贴了这张卡的工作流会失去作用（引用仍留在数据里）。`)) return
    if (kind === 'agent') setAgents((p) => p.filter((c) => c.id !== sel))
    else setSkills((p) => p.filter((c) => c.id !== sel))
    setSel('')
    setDirty(true)
  }

  const save = async () => {
    try {
      const r = await fetch('/api/cards', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agents, skills }),
      })
      const d = await r.json().catch(() => ({}))
      if (!r.ok) {
        setMsg({ text: (d as { error?: string }).error || `保存失败（${r.status}）`, bad: true })
        return
      }
      setMsg({ text: '已保存到 data/cards.json' })
      setDirty(false)
      onSaved()
    } catch (e) {
      setMsg({ text: String(e), bad: true })
    }
  }

  return (
    <div
      className="fixed inset-0 z-[60] flex items-center justify-center"
      style={{ background: 'rgba(4,7,12,0.7)' }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        className="w-[780px] h-[560px] flex flex-col rounded-xl border border-border bg-surface shadow-2xl overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-3 px-4 py-2.5 border-b border-border">
          <span className="text-sm text-white">卡片管理</span>
          <div className="flex gap-1">
            {(['agent', 'skill'] as const).map((k) => (
              <button
                key={k}
                onClick={() => {
                  setKind(k)
                  setSel((k === 'agent' ? agents : skills)[0]?.id || '')
                }}
                className={`px-2.5 py-1 rounded-md text-[11px] border ${
                  kind === k ? 'bg-canvas border-border text-white' : 'border-transparent text-gray-500'
                }`}
              >
                {k === 'agent' ? `智能体 ${agents.length}` : `技能 ${skills.length}`}
              </button>
            ))}
          </div>
          <div className="flex-1" />
          <button onClick={onClose} className="fe-drawer__x">
            ✕
          </button>
        </div>

        <div className="flex flex-1 min-h-0">
          {/* 左列 */}
          <div className="w-[210px] border-r border-border flex flex-col">
            <div className="p-2">
              <button onClick={add} className="w-full px-2 py-1.5 rounded-md border border-border text-[11px] text-gray-300 hover:border-primary/60">
                + 新建
              </button>
            </div>
            <div className="flex-1 overflow-y-auto px-2 pb-2">
              {(list as (AgentCard | SkillCard)[]).map((c) => (
                <button
                  key={c.id}
                  onClick={() => setSel(c.id)}
                  className={`w-full text-left px-2 py-1.5 rounded-md mb-1 border text-[11px] ${
                    sel === c.id ? 'bg-canvas border-primary/50 text-white' : 'border-transparent text-gray-400 hover:bg-canvas/60'
                  }`}
                >
                  <span className="block truncate">{c.name}</span>
                  <span className="block text-[9px] text-gray-600 truncate">{c.category || '未分类'} · {c.id}</span>
                </button>
              ))}
            </div>
          </div>

          {/* 右列：表单 */}
          <div className="flex-1 overflow-y-auto p-3">
            {!cur ? (
              <p className="text-[11px] text-gray-500">左侧选一张卡，或点「+ 新建」</p>
            ) : (
              <div className="flex flex-col gap-2.5">
                <Field label="名字">
                  <input value={cur.name} onChange={(e) => patch({ name: e.target.value })} className={INPUT} />
                </Field>
                <div className="grid grid-cols-2 gap-2">
                  <Field label="id（工作流引用它，改名不要改 id）">
                    <input
                      value={cur.id}
                      onChange={(e) => {
                        const next = e.target.value.trim()
                        if (!next) return
                        setDirty(true)
                        if (kind === 'agent') setAgents((p) => p.map((c) => (c.id === sel ? { ...c, id: next } : c)))
                        else setSkills((p) => p.map((c) => (c.id === sel ? { ...c, id: next } : c)))
                        setSel(next)
                      }}
                      className={`${INPUT} font-mono`}
                    />
                  </Field>
                  <Field label="大类">
                    <input value={cur.category || ''} onChange={(e) => patch({ category: e.target.value })} className={INPUT} />
                  </Field>
                </div>
                <Field label="标签（逗号分隔，用于搜索）">
                  <input
                    value={(cur.tags || []).join(',')}
                    onChange={(e) => patch({ tags: e.target.value.split(/[,，]/).map((x) => x.trim()).filter(Boolean) })}
                    className={INPUT}
                  />
                </Field>
                {kind === 'agent' ? (
                  <Field label="角色（一句话，显示在库里）">
                    <input value={(cur as AgentCard).role || ''} onChange={(e) => patch({ role: e.target.value })} className={INPUT} />
                  </Field>
                ) : (
                  <div className="grid grid-cols-2 gap-2">
                    <Field label="说明">
                      <input value={(cur as SkillCard).desc || ''} onChange={(e) => patch({ desc: e.target.value })} className={INPUT} />
                    </Field>
                    <Field label="产出形态">
                      <select value={(cur as SkillCard).out_kind || 'text'} onChange={(e) => patch({ out_kind: e.target.value })} className={INPUT}>
                        <option value="text">text</option>
                        <option value="json">json</option>
                      </select>
                    </Field>
                  </div>
                )}
                {kind === 'skill' && (
                  <div>
                    <Field label="吃什么（看图卡只能贴图节点）">
                      <select
                        value={(cur as SkillCard).media || ''}
                        onChange={(e) => patch({ media: e.target.value })}
                        className={INPUT}
                      >
                        <option value="">纯文本</option>
                        <option value="image">图片（走视觉模型）</option>
                      </select>
                    </Field>
                  </div>
                )}
                <Field label="系统提示词（贴到节点上就是这段）">
                  <textarea
                    value={cur.system || ''}
                    onChange={(e) => patch({ system: e.target.value })}
                    rows={7}
                    className={`${INPUT} resize-y font-mono text-[11px] leading-relaxed`}
                  />
                </Field>
                {kind === 'agent' && (
                  <Field label="默认工具（节点自己勾了则以节点为准）">
                    <div className="flex flex-wrap gap-1.5">
                      {TOOLS.map((t) => {
                        const on = ((cur as AgentCard).tools || []).includes(t.id)
                        return (
                          <button
                            key={t.id}
                            onClick={() =>
                              patch({
                                tools: on
                                  ? ((cur as AgentCard).tools || []).filter((x) => x !== t.id)
                                  : [...((cur as AgentCard).tools || []), t.id],
                              })
                            }
                            className={`px-2 py-0.5 rounded text-[10px] border ${
                              on ? 'bg-ai/20 border-ai/50 text-indigo-200' : 'border-border text-gray-500'
                            }`}
                          >
                            {t.label}
                          </button>
                        )
                      })}
                    </div>
                  </Field>
                )}
                <div className="flex items-center gap-2 pt-1">
                  <button
                    onClick={dup}
                    className="px-2 py-1 rounded text-[10px] border border-border text-gray-400 hover:text-white"
                  >
                    复制一份
                  </button>
                  <button
                    onClick={del}
                    className="px-2 py-1 rounded text-[10px] border border-red-900/50 text-red-400 hover:bg-red-950/30"
                  >
                    删除这张卡
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>

        <div className="flex items-center gap-3 px-4 py-2.5 border-t border-border">
          <button
            onClick={save}
            disabled={!dirty}
            className={`px-3 py-1.5 rounded-md text-[11px] ${
              dirty ? 'bg-primary/25 text-indigo-200 border border-primary/50' : 'bg-canvas text-gray-600 border border-border'
            }`}
          >
            保存全部
          </button>
          {dirty && <span className="text-[10px] text-amber-500/90">有未保存的修改</span>}
          {msg && <span className={`text-[10px] ${msg.bad ? 'text-error' : 'text-success'}`}>{msg.text}</span>}
          <div className="flex-1" />
          <span className="text-[10px] text-gray-600">改动写入 data/cards.json，引擎即时生效（无需重启）</span>
        </div>
      </div>
    </div>
  )
}

const INPUT =
  'w-full px-2 py-1 rounded-md bg-canvas border border-border text-[11px] outline-none focus:border-primary'

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
      {children}
    </div>
  )
}
