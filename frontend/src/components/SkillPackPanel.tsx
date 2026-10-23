import { useEffect, useMemo, useState } from 'react'

import {
  fetchSkillDoc,
  fetchSkillPacks,
  humanBytes,
  sortPacks,
  tierLabel,
  tierTone,
  verdictLabel,
  verdictTone,
  type SkillPack,
  type SkillPackList,
} from '../lib/skillpacks'

// 技能包索引面板：只读审计，不产生可贴的能力。
//
// 三个视图对应三个问题（不是三个随便玩的筛选器）：
//   通用能力 —— 平台默认给我什么
//   全部收录 —— 我要做某个领域的图，有没有现成经验（领域包在这里查得到）
//   含停用   —— 某个包为什么不算数
//
// 领域包刻意不进第一个视图：平台是通用产物，默认推荐里不该出现「UE 资产六道门」
// 这种只对某个工程成立的预设；但用户主动来查时必须查得到，并且能直接用。

type View = 'core' | 'retained' | 'everything'

const VIEWS: { key: View; label: string }[] = [
  { key: 'core', label: '通用能力' },
  { key: 'retained', label: '全部收录' },
  { key: 'everything', label: '含停用' },
]

export default function SkillPackPanel() {
  const [data, setData] = useState<SkillPackList | null>(null)
  const [loading, setLoading] = useState(true)
  const [view, setView] = useState<View>('retained')
  const [open, setOpen] = useState('')
  const [doc, setDoc] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    setLoading(true)
    const q = view === 'core' ? { tier: 'core' as const } : view === 'everything' ? { includeNone: true } : {}
    fetchSkillPacks(q)
      .then((d) => {
        if (alive) setData(d)
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [view])

  const packs = useMemo(() => sortPacks(data?.packs || []), [data])

  const toggle = async (p: SkillPack) => {
    if (open === p.name) {
      setOpen('')
      setDoc(null)
      return
    }
    setOpen(p.name)
    setDoc(null)
    const text = await fetchSkillDoc(p.name)
    setDoc(text ?? '（读不到正文）')
  }

  if (!data && loading) return <p className="p-4 text-[11px] text-gray-500">正在扫码…</p>
  if (!data) {
    return (
      <p className="p-4 text-[11px] text-gray-500">
        技能包索引不可用（引擎未启动，或 /api/skills 未接线）
      </p>
    )
  }

  const s = data.summary
  return (
    <div className="flex flex-col flex-1 overflow-hidden">
      {/* 视图切换 + 汇总 */}
      <div className="px-4 py-2 flex items-center gap-3 border-b border-border text-[10px] flex-wrap">
        <div className="flex gap-1">
          {VIEWS.map((v) => (
            <button
              key={v.key}
              onClick={() => setView(v.key)}
              className={`px-2 py-0.5 rounded-full border transition-all ${
                view === v.key
                  ? 'bg-primary/20 border-primary/50 text-indigo-200'
                  : 'border-border text-gray-500 hover:text-gray-300'
              }`}
            >
              {v.label}
            </button>
          ))}
        </div>
        <span className="text-gray-600">|</span>
        <span className="text-gray-400">
          扫到 <b className="text-gray-200">{s.total}</b>
        </span>
        <span className="text-indigo-300">
          通用 <b>{s.core}</b>
        </span>
        <span className="text-amber-300">
          领域 <b>{s.domain}</b>
        </span>
        <span className="text-teal-300">
          提炼卡 <b>{s.cards}</b>
        </span>
        {s.plugins ? (
          <span className="text-gray-400">
            专家插件 <b>{s.plugins}</b>
          </span>
        ) : null}
        <span className={data.verified ? 'ml-auto text-gray-600' : 'ml-auto text-amber-400'}>
          {data.verified ? '证据已核验' : '本机无源码，证据未核验'}
        </span>
      </div>

      {data.problems && data.problems.length > 0 && (
        <div className="px-4 py-1.5 text-[10px] text-amber-300 border-b border-border">
          {data.problems.length} 条判定核验未通过（已降级为未判定）：{data.problems[0]}
        </div>
      )}

      <div className="flex-1 overflow-y-auto p-3 space-y-1.5">
        {packs.map((p) => (
          <div
            key={p.name}
            className={`rounded-lg border bg-canvas/40 ${
              p.tier === 'none' ? 'border-border/40 opacity-60' : 'border-border'
            }`}
          >
            <button onClick={() => toggle(p)} className="w-full text-left px-2.5 py-2 flex items-start gap-2">
              <span className={`shrink-0 mt-0.5 px-1.5 py-0.5 rounded text-[9px] border ${tierTone(p.tier)}`}>
                {tierLabel(p.tier)}
              </span>
              <span className="flex-1 min-w-0">
                <span className="block text-[11px] text-gray-200 font-medium truncate">
                  {p.name}
                  <span className={`ml-1.5 px-1 py-px rounded border text-[9px] ${verdictTone(p.verdict)}`}>
                    {verdictLabel(p.verdict)}
                  </span>
                  {/* 来源：全盘有五处本机技能生态 + 专家插件市场，不标来源就回答不了「从哪扫的」 */}
                  {p.source && (
                    <span className="ml-1.5 px-1 py-px rounded border border-border text-[9px] text-gray-500">
                      {p.source}
                    </span>
                  )}
                </span>
                <span className="block text-[10px] text-gray-500 leading-snug">{p.reason}</span>
              </span>
              <span className="shrink-0 text-[9px] text-gray-600 text-right">
                {humanBytes(p.bytes)}
                {p.refs && p.refs.length > 0 && <> · {p.refs.length} 篇参考</>}
                {p.card && <span className="block text-teal-400">卡：{p.card}</span>}
              </span>
            </button>

            {open === p.name && (
              <div className="px-2.5 pb-2 border-t border-border/60">
                {/* 领域包的用法必须写出来，否则用户不知道"收录了"到底怎么用 */}
                <div className="mt-1.5 text-[9px] text-gray-500">
                  引用方式：在<b className="text-gray-300">外部智能体</b>节点的
                  <b className="text-gray-300">技能包参考</b>里填 <code className="text-sky-300">{p.name}</code>
                  ，执行时会把 SKILL.md 与参考文献的路径写进任务描述，让智能体自己去读
                </div>
                {p.evidence && p.evidence.length > 0 && (
                  <div className="mt-1.5">
                    <span className="text-[9px] text-gray-500">证据（文件:行#锚点）</span>
                    <ul className="mt-0.5 space-y-0.5">
                      {p.evidence.map((e) => (
                        <li key={e} className="text-[9px] text-sky-300/80 font-mono truncate">
                          {e}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
                {p.read_when && p.read_when.length > 0 && (
                  <div className="mt-1.5">
                    <span className="text-[9px] text-gray-500">适用场景</span>
                    <p className="text-[9px] text-gray-400">{p.read_when.join(' / ')}</p>
                  </div>
                )}
                <div className="mt-1.5">
                  <span className="text-[9px] text-gray-500">正文</span>
                  <pre className="mt-0.5 max-h-52 overflow-auto text-[9px] leading-relaxed text-gray-400 whitespace-pre-wrap">
                    {doc === null ? '读取中…' : doc.slice(0, 6000)}
                  </pre>
                </div>
              </div>
            )}
          </div>
        ))}
        {packs.length === 0 && (
          <p className="text-[11px] text-gray-500">
            这个视图下没有包。扫描目录：{[...data.roots, ...(data.plugin_roots || [])].join(' ; ') || '（无）'}
          </p>
        )}
      </div>

      <p className="px-4 py-2 text-[10px] text-gray-500 border-t border-border">
        这一屏是审计结果，不是可贴的能力。贴卡请用「智能体库 / 技能库」；领域包靠节点上的「技能包参考」引用。
      </p>
    </div>
  )
}
