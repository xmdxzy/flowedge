import { useMemo, useState } from 'react'

import CardEditor from './CardEditor'
import SkillPackPanel from './SkillPackPanel'

import {
  cardName,
  STAGE_HINT,
  STAGE_LABEL,
  type Binding,
  type CardCatalog,
  type CardKind,
  type Stage,
} from '../lib/cards'

// 能力库：智能体库 / 技能库 / 工作流库，分大类 + 细类标签 + 搜索。
//
// 为什么不做成三个平铺 tab：卡片会越来越多（工作流卡是用户自己的，几十条起步）。
// 平铺列表在第 30 条之后就没法用了；库 = 大类筛选 + 关键词，才撑得住规模。
//
// 为什么用弹层而不是在 288px 面板里挤：选卡是"浏览 + 比较"的动作，
// 需要横向空间放两列卡片；选完立刻关闭回到面板。

interface Props {
  catalog: CardCatalog | null
  stage: Stage
  onPick: (b: Binding) => void
  onClose: () => void
  /** 卡片被编辑后，让上层重新拉一次目录 */
  onReload?: () => void
}

// 第四个页签是「技能包」——它**不是**卡片库的一类。
// 卡片是能贴到节点上的能力；技能包是"机器上那些 SKILL.md 到底算不算数"的审计。
// 混进 CardKind 会让"贴卡"这条路径拿到一个贴不上去的 kind，所以单独一个 LibKey。
type LibKey = CardKind | 'pack'

const LIBS: { kind: LibKey; label: string }[] = [
  { kind: 'agent', label: '智能体库' },
  { kind: 'skill', label: '技能库' },
  { kind: 'workflow', label: '工作流库' },
  { kind: 'pack', label: '技能包' },
]

export default function CardLibrary({ catalog, stage, onPick, onClose, onReload }: Props) {
  // 默认打开哪一库：替换段只有工作流，本体段是角色（智能体），输入/输出段多是技能。
  const [kind, setKind] = useState<LibKey>(
    stage === 'replace' ? 'workflow' : stage === 'body' ? 'agent' : 'skill',
  )
  const [editing, setEditing] = useState(false)
  // 保存后让上层重新拉一次目录（库是 App 持有的 state）
  const [reloadKey, setReloadKey] = useState(0)
  const [cat, setCat] = useState('')
  const [q, setQ] = useState('')

  const categories = useMemo(() => {
    if (kind === 'pack') return [] as string[]
    const list = catalog?.categories?.[kind] || []
    return list.filter(Boolean)
  }, [catalog, kind])

  const items = useMemo(() => {
    if (!catalog) return [] as { id: string; name: string; sub: string; icon: string; cat: string; tags: string[] }[]
    // 技能包不是卡片，没有可贴的条目
    if (kind === 'pack') return [] as { id: string; name: string; sub: string; icon: string; cat: string; tags: string[] }[]
    const kw = q.trim().toLowerCase()
    const match = (name: string, sub: string, c: string, tags: string[] = []) => {
      if (cat && c !== cat) return false
      if (!kw) return true
      return `${name} ${sub} ${c} ${tags.join(' ')}`.toLowerCase().includes(kw)
    }
    if (kind === 'agent') {
      return catalog.agents
        .filter((a) => match(a.name, a.role, a.category, a.tags))
        .map((a) => ({ id: a.id, name: a.name, sub: a.role, icon: a.icon || '', cat: a.category, tags: a.tags || [] }))
    }
    if (kind === 'skill') {
      return catalog.skills
        .filter((s) => match(s.name, s.desc, s.category, s.tags))
        .map((s) => ({
          id: s.id,
          name: s.name,
          sub: `${s.media === 'image' ? '［看图］' : ''}${s.output_hint ? `${s.desc} · 产出 ${s.output_hint}` : s.desc}`,
          icon: s.icon || '',
          cat: s.category,
          tags: s.tags || [],
        }))
    }
    // 工作流按规模分两桶，与后端 categories 的口径必须一致，
    // 否则选中"轻量"会一条都筛不出来（这个 bug 我第一版就写出来了）。
    const bucket = (n: number) => (n <= 5 ? '轻量（≤5 节点）' : '完整流水线（>5 节点）')
    return catalog.workflows
      .filter((w) => match(w.name, w.desc, bucket(w.nodes), []))
      .map((w) => ({
        id: w.id,
        name: w.name,
        sub: `${w.nodes} 个节点 · 整条流水线`,
        icon: '',
        cat: bucket(w.nodes),
        tags: [],
      }))
  }, [catalog, kind, cat, q])

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center"
      style={{ background: 'rgba(4,7,12,0.62)', backdropFilter: 'blur(2px)' }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        className="w-[660px] max-h-[78vh] flex flex-col rounded-xl border border-border bg-surface shadow-2xl overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部：库切换 */}
        <div className="flex items-center gap-3 px-4 pt-3 pb-2 border-b border-border">
          <div className="flex gap-1 flex-1">
            {LIBS.map((l) => (
              <button
                key={l.kind}
                onClick={() => {
                  setKind(l.kind)
                  setCat('')
                }}
                className={`px-3 py-1.5 rounded-lg text-xs transition-all ${
                  kind === l.kind
                    ? 'bg-canvas text-white border border-border'
                    : 'text-gray-500 hover:text-gray-300 border border-transparent'
                }`}
              >
                {l.label}
              </button>
            ))}
          </div>
          <span className="text-[10px] text-gray-500">
            要加到：<b className="text-gray-300">{STAGE_LABEL[stage]}</b>
          </span>
          <button
            onClick={() => setEditing(true)}
            className={`text-[10px] text-gray-400 hover:text-white px-1.5 py-0.5 rounded border border-border ${
              kind === 'pack' ? 'hidden' : ''
            }`}
            title="新建/修改智能体与技能卡"
          >
            管理卡片
          </button>
          <button onClick={onClose} className="fe-drawer__x">
            ✕
          </button>
        </div>

        {/* 技能包：审计视图，没有筛选条也没有可贴的条目 */}
        {kind === 'pack' ? (
          <SkillPackPanel />
        ) : (
          <>
            {/* 筛选条：搜索 + 大类 */}
            <div className="px-4 py-2 flex items-center gap-2 border-b border-border">
              <input
                autoFocus
                value={q}
                onChange={(e) => setQ(e.target.value)}
                placeholder="搜索名称 / 用途 / 标签…"
                className="w-44 px-2 py-1 rounded-lg bg-canvas border border-border text-[11px] outline-none focus:border-primary"
              />
              <div className="flex gap-1 flex-wrap flex-1">
                <Chip on={cat === ''} onClick={() => setCat('')}>
                  全部 {catalog ? (kind === 'agent' ? catalog.agents.length : kind === 'skill' ? catalog.skills.length : catalog.workflows.length) : 0}
                </Chip>
                {categories.map((c) => (
                  <Chip key={c} on={cat === c} onClick={() => setCat(c)}>
                    {c}
                  </Chip>
                ))}
              </div>
            </div>

            {/* 卡片列表 */}
            <div className="p-3 overflow-y-auto grid grid-cols-2 gap-2 flex-1">
              {!catalog && <p className="text-[11px] text-gray-500 col-span-2">卡片目录未加载（引擎未启动？）</p>}
              {catalog && items.length === 0 && (
                <p className="text-[11px] text-gray-500 col-span-2">没有匹配的卡片，换个关键词或大类</p>
              )}
              {items.map((it) => (
                <button
                  key={it.id}
                  onClick={() => {
                    onPick({ kind: kind as CardKind, id: it.id, stage })
                    onClose()
                  }}
                  className={`fe-card-item fe-card-item--${kind} text-left`}
                >
                  <span className={`fe-card-mark fe-card-mark--${kind}`}>{kind === 'workflow' ? <i /> : it.icon}</span>
                  <span className="fe-card-body">
                    <span className="fe-card-name">
                      {it.name}
                      {it.cat && <span className="ml-1 text-[9px] text-gray-500">{it.cat}</span>}
                    </span>
                    <span className="fe-card-sub">{it.sub}</span>
                    {it.tags.length > 0 && (
                      <span className="block text-[9px] text-gray-600 truncate">
                        {it.tags.slice(0, 3).map((t) => `#${t}`).join(' ')}
                      </span>
                    )}
                  </span>
                </button>
              ))}
            </div>

            {/* 底部说明：这一段的语义 */}
            <p className="px-4 py-2 text-[10px] text-gray-500 border-t border-border">{STAGE_HINT[stage]}</p>
          </>
        )}
      </div>

      {editing && (
        <CardEditor
          key={reloadKey}
          catalog={catalog}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setReloadKey((k) => k + 1)
            onReload?.()
          }}
        />
      )}
    </div>
  )
}

function Chip({ on, onClick, children }: { on: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={`px-2 py-0.5 rounded-full text-[10px] border transition-all ${
        on ? 'bg-primary/20 border-primary/50 text-indigo-200' : 'border-border text-gray-500 hover:text-gray-300'
      }`}
    >
      {children}
    </button>
  )
}

export function cardLabel(kind: CardKind, id: string) {
  return cardName(kind, id)
}
