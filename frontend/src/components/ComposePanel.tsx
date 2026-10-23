import { useState } from 'react'
import type { Edge, Node } from '@xyflow/react'

// 构图面板：一句话目标 → 图 + 任务书。
//
// 这个面板存在的理由：`POST /api/compose` 落地后只有 API，等于"能力存在但用户点不到"
// （项目里已经踩过一次：5 个新能力全部 0 暴露）。
//
// 三个设计取舍：
//   - **生成是提案，不是执行**。面板上没有任何"跑起来"的按钮，因为出片是分钟到小时级。
//     生成 → 看一眼 → 载入画布 → 自己决定要不要跑。
//   - **失败要给出违规明细**，而不是"生成失败"。违规就是模型的错误清单，
//     看它才知道该改目标措辞还是该补模块。
//   - **任务书与图一起给**。只有图的话，人还得自己导出一次才知道这图要满足什么。

interface DraftNode {
  id: string
  type: string
  data?: Record<string, unknown>
  position?: { x: number; y: number }
}

interface DraftEdge {
  id?: string
  source: string
  target: string
  sourceHandle?: string | null
  targetHandle?: string | null
}

interface ComposeResult {
  rounds?: number
  legal?: boolean
  score?: { score: number; legal: boolean; nodes: number; layers: number }
  summary?: string
  violations?: string[]
  lessons?: string[]
  nodeCount?: number
  edgeCount?: number
  draft?: { nodes: DraftNode[]; edges: DraftEdge[] }
  spec?: string
  specError?: string
  workflowId?: number
  error?: string
}

export default function ComposePanel({
  open,
  onClose,
  onLoad,
  onNotify,
}: {
  open: boolean
  onClose: () => void
  onLoad?: (nodes: Node[], edges: Edge[], name: string) => void
  onNotify?: (m: string) => void
}) {
  const [goal, setGoal] = useState('')
  const [seconds, setSeconds] = useState('')
  const [aspect, setAspect] = useState('')
  const [noSpeech, setNoSpeech] = useState(false)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<ComposeResult | null>(null)
  const [err, setErr] = useState('')
  const [showSpec, setShowSpec] = useState(false)
  const [saving, setSaving] = useState(false)

  if (!open) return null

  const run = async () => {
    if (!goal.trim()) {
      setErr('先写一句目标：要做出什么、多长、什么画幅。')
      return
    }
    setBusy(true)
    setErr('')
    setResult(null)
    try {
      const body: Record<string, unknown> = { goal: goal.trim(), maxRepairRounds: 2 }
      if (seconds.trim()) body.totalSeconds = Number(seconds)
      if (aspect.trim()) body.aspect = aspect.trim()
      if (noSpeech) body.noSpeech = true
      const r = await fetch('/api/compose', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      const d: ComposeResult = await r.json()
      // 后端在"生成多轮仍不合法"时返回 422 + violations，那不是"请求失败"，
      // 是"模型没画对" —— 要展示明细而不是只说 HTTP 422。
      if (!r.ok && !d.violations?.length) {
        throw new Error(d.error || 'HTTP ' + r.status)
      }
      setResult(d)
    } catch (e) {
      setErr('生成失败：' + String(e))
    } finally {
      setBusy(false)
    }
  }

  /** 把草稿转成画布节点/边。后端已按拓扑层给了坐标；缺坐标时兜底排一行，不让节点叠在一起。 */
  const toCanvas = (): { nodes: Node[]; edges: Edge[] } => {
    const d = result?.draft
    if (!d) return { nodes: [], edges: [] }
    const nodes: Node[] = (d.nodes || []).map((n, i) => ({
      id: n.id,
      type: n.type,
      position: n.position ?? { x: 80 + i * 240, y: 200 },
      data: (n.data || {}) as Record<string, unknown>,
    }))
    const edges: Edge[] = (d.edges || []).map((e, i) => ({
      id: e.id || `e-${e.source}-${e.target}-${i}`,
      source: e.source,
      target: e.target,
      ...(e.sourceHandle ? { sourceHandle: e.sourceHandle } : {}),
      ...(e.targetHandle ? { targetHandle: e.targetHandle } : {}),
    }))
    return { nodes, edges }
  }

  const nameOf = () => '构图：' + goal.trim().slice(0, 24)

  const loadToCanvas = () => {
    const { nodes, edges } = toCanvas()
    if (nodes.length === 0) {
      setErr('没有可载入的图')
      return
    }
    onLoad?.(nodes, edges, nameOf())
    onNotify?.(`已载入 ${nodes.length} 个节点到画布（未保存）`)
    onClose()
  }

  /** 保存走普通的工作流 CRUD，不重复调 compose —— 重新生成会得到另一张图。 */
  const saveWorkflow = async () => {
    const { nodes, edges } = toCanvas()
    if (nodes.length === 0) return
    setSaving(true)
    try {
      const r = await fetch('/api/workflows', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: nameOf(), nodes, edges }),
      })
      if (!r.ok) throw new Error('HTTP ' + r.status)
      const d = await r.json()
      onNotify?.(`已保存为工作流 #${d.id ?? ''}`)
      onClose()
    } catch (e) {
      setErr('保存失败：' + String(e))
    } finally {
      setSaving(false)
    }
  }

  const violations = result?.violations || []
  const ok = result && !result.error && (result.legal || result.draft)

  return (
    <div className="fixed inset-0 z-40 flex" onClick={onClose}>
      <div
        className="ml-auto h-full w-[600px] max-w-[94vw] flex flex-col fe-drawer fe-scroll-thin overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="fe-drawer__hd sticky top-0 z-10">
          <span className="text-[13px] font-semibold text-white">构图</span>
          <span className="text-[10px] text-gray-500">目标 → 图 → 任务书（不执行）</span>
          <button className="ml-auto fe-drawer__x" onClick={onClose}>
            ×
          </button>
        </div>

        <div className="p-4 flex flex-col gap-3">
          <div>
            <label className="text-[11px] text-gray-400 block mb-1">要做什么</label>
            <textarea
              value={goal}
              onChange={(e) => setGoal(e.target.value)}
              className="w-full h-20 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
              placeholder="例：30 秒竖屏氛围短片，夜雨空镜，无台词"
            />
            <p className="text-[10px] text-gray-600 mt-1 leading-relaxed">
              写得越具体越准：时长、画幅、有没有台词、素材是已有文件还是要生成。
              「已有文件」会走文件源，需要你后面自己填路径。
            </p>
          </div>

          <div className="flex items-end gap-2">
            <div className="w-24">
              <label className="text-[10px] text-gray-500 block mb-1">总时长(秒)</label>
              <input
                value={seconds}
                onChange={(e) => setSeconds(e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none mono-num"
                placeholder="30"
              />
            </div>
            <div className="w-24">
              <label className="text-[10px] text-gray-500 block mb-1">画幅</label>
              <select
                value={aspect}
                onChange={(e) => setAspect(e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
              >
                <option value="">不约束</option>
                <option value="9:16">9:16 竖屏</option>
                <option value="16:9">16:9 横屏</option>
                <option value="1:1">1:1 方图</option>
              </select>
            </div>
            <label className="flex items-center gap-1.5 text-[11px] text-gray-400 pb-2">
              <input type="checkbox" checked={noSpeech} onChange={(e) => setNoSpeech(e.target.checked)} />
              无台词
            </label>
            <button
              onClick={() => void run()}
              disabled={busy}
              className="ml-auto px-3 py-1.5 rounded-lg bg-primary/20 border border-primary/40 text-[12px] text-data hover:bg-primary/30 disabled:opacity-40"
            >
              {busy ? '生成中…' : '生成图'}
            </button>
          </div>

          {busy && (
            <p className="text-[10px] text-gray-500">
              通常 5–35 秒。不合法时会带着违规清单自动改图，最多 2 轮。
            </p>
          )}

          {err && (
            <p className="text-[11px] text-red-400 leading-relaxed rounded-lg border border-red-500/30 bg-red-500/10 px-2 py-1.5">
              {err}
            </p>
          )}

          {result && (
            <div className="flex flex-col gap-3 pt-1 border-t border-border">
              <div className="flex items-center gap-3 pt-2">
                <span
                  className={
                    'px-2 py-0.5 rounded text-[10px] ' +
                    (result.legal
                      ? 'bg-emerald-500/15 text-emerald-300 border border-emerald-500/30'
                      : 'bg-amber-500/15 text-amber-300 border border-amber-500/30')
                  }
                >
                  {result.legal ? '结构合法' : '结构不合法'}
                </span>
                <span className="mono-num text-[11px] text-gray-400">
                  {result.nodeCount ?? 0} 节点 · {result.edgeCount ?? 0} 边 · {result.rounds ?? 1} 轮
                </span>
                {result.score && (
                  <span className="mono-num text-[11px] text-gray-500">结构分 {result.score.score.toFixed(2)}</span>
                )}
              </div>

              {violations.length > 0 && (
                <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-2 py-1.5">
                  <p className="text-[10px] text-amber-300 mb-1">
                    生成过程中出现过这些问题（最后一张图是修过之后的）：
                  </p>
                  <ul className="text-[10px] text-amber-200/80 leading-relaxed list-disc pl-4">
                    {violations.slice(0, 8).map((v, i) => (
                      <li key={i}>{v}</li>
                    ))}
                  </ul>
                </div>
              )}

              {result.draft?.nodes?.length ? (
                <div>
                  <p className="text-[10px] text-gray-500 mb-1">节点</p>
                  <div className="flex flex-wrap gap-1">
                    {result.draft.nodes.map((n) => (
                      <span
                        key={n.id}
                        className="px-1.5 py-0.5 rounded bg-surface-hover/50 border border-border text-[10px] text-gray-300"
                      >
                        {n.type}
                      </span>
                    ))}
                  </div>
                </div>
              ) : null}

              {result.spec ? (
                <div>
                  <button
                    className="text-[11px] text-data hover:underline"
                    onClick={() => setShowSpec((v) => !v)}
                  >
                    {showSpec ? '收起任务书' : `查看任务书（${result.spec.length} 字）`}
                  </button>
                  {showSpec && (
                    <pre className="mt-1 max-h-72 overflow-auto fe-scroll-thin rounded-lg bg-black/40 border border-border p-2 text-[10px] leading-relaxed text-gray-300 whitespace-pre-wrap">
                      {result.spec}
                    </pre>
                  )}
                </div>
              ) : result.specError ? (
                <p className="text-[10px] text-gray-500">任务书编译失败：{result.specError}</p>
              ) : null}

              {ok && (
                <div className="flex gap-2 pt-1">
                  <button
                    onClick={loadToCanvas}
                    className="px-3 py-1.5 rounded-lg bg-primary/20 border border-primary/40 text-[12px] text-data hover:bg-primary/30"
                  >
                    载入画布
                  </button>
                  <button
                    onClick={() => void saveWorkflow()}
                    disabled={saving}
                    className="px-3 py-1.5 rounded-lg bg-surface-hover/50 border border-border text-[12px] text-gray-300 hover:bg-white/10 disabled:opacity-40"
                  >
                    {saving ? '保存中…' : '直接保存'}
                  </button>
                  <span className="text-[10px] text-gray-600 self-center">
                    载入后不会自动跑，跑不跑你决定
                  </span>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
