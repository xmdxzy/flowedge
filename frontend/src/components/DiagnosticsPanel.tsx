import { useCallback, useEffect, useState } from 'react'

// 诊断面板：把「这批 run 哪里在漏」摆到界面上，并允许一键生成候选去试。
//
// 设计取舍：
//   - 不要做成"评分仪表盘"。人不需要看 CR 曲线，需要的是**下一步做什么**。
//     所以主视觉是诊断卡片（带具体数值与建议），总览三数退到次要位置。
//   - 候选永远要显示 from → to。只说"建议改成 -14"是命令，
//     说"从 -20 改成 -14，依据是平台响度基准"才是可判断的建议。
//   - "应用"与"应用并重跑"分开：出片成本高，跑不跑是人的决定。

interface GateStat {
  name: string
  level: string
  total: number
  failed: number
  failRate: number
}

interface WorkflowStat {
  workflowId: number
  name: string
  runs: number
  passed: number
  failed: number
  aborted: number
  unscored: number
  crAll: number
  crClean: number | null
  abortedRate: number
  unscoredRate: number
  avgMs: number
  p95Ms: number
  topFailingGates?: GateStat[]
  hasEvidence: boolean
}

interface Diagnosis {
  severity: string
  kind: string
  workflowId?: number
  target: string
  message: string
  suggestion: string
  evidence?: number[]
  metric?: string
}

interface Report {
  total: number
  fromEvidence: number
  fromHistory: number
  overall: WorkflowStat
  workflows: WorkflowStat[]
  diagnoses: Diagnosis[]
  trustworthy: boolean
  note?: string
}

interface CandidateEdit {
  nodeId: string
  path: string
  from: unknown
  to: unknown
  why: string
}

interface Candidate {
  id: string
  workflowId: number
  title: string
  diagnosisKind: string
  edits: CandidateEdit[]
  expected: string
  risk: string
  confidence: string
  basis: string
}

const SEV_STYLE: Record<string, string> = {
  P0: 'border-l-[3px] border-l-red-400 bg-red-500/[0.06]',
  P1: 'border-l-[3px] border-l-amber-400 bg-amber-500/[0.05]',
  P2: 'border-l-[3px] border-l-sky-400/70 bg-sky-500/[0.04]',
}

function fmtDur(ms: number): string {
  if (!ms) return '—'
  if (ms < 1000) return ms + 'ms'
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's'
  return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's'
}

function pct(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—'
  return (v * 100).toFixed(0) + '%'
}

export default function DiagnosticsPanel({
  open,
  onClose,
  onNotify,
}: {
  open: boolean
  onClose: () => void
  onNotify?: (m: string) => void
}) {
  const [report, setReport] = useState<Report | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [cands, setCands] = useState<Candidate[] | null>(null)
  const [candFor, setCandFor] = useState<Diagnosis | null>(null)
  const [candNote, setCandNote] = useState('')
  const [busy, setBusy] = useState('')

  const refresh = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const r = await fetch('/api/evidence/summary?limit=300')
      if (!r.ok) throw new Error('HTTP ' + r.status)
      const d = await r.json()
      setReport(d.report)
    } catch (e) {
      setError('拉取诊断失败：' + String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (open) void refresh()
  }, [open, refresh])

  /** 生成候选：只读，不动图 */
  const propose = async (d: Diagnosis) => {
    if (!d.workflowId) {
      onNotify?.('这条诊断是全局的，没有可改的图')
      return
    }
    setBusy('propose')
    setCands(null)
    setCandFor(d)
    try {
      const r = await fetch('/api/evidence/candidates', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ workflowId: d.workflowId, diagnosis: d }),
      })
      const j = await r.json()
      if (!r.ok) throw new Error(j.error || 'HTTP ' + r.status)
      setCands(j.candidates || [])
      setCandNote(j.note || '')
    } catch (e) {
      onNotify?.('生成候选失败：' + String(e))
    } finally {
      setBusy('')
    }
  }

  /** 应用候选（可选立即重跑） */
  const trial = async (c: Candidate, run: boolean) => {
    if (!c.edits.length) {
      onNotify?.('这条候选不带参数改动（只是结构检查建议）')
      return
    }
    setBusy(run ? 'run' : 'apply')
    try {
      const r = await fetch('/api/evidence/trial', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          workflowId: c.workflowId,
          edits: c.edits,
          run,
          note: c.title,
        }),
      })
      const j = await r.json()
      if (!r.ok) throw new Error(j.error || 'HTTP ' + r.status)
      const msg = `已应用 ${j.count} 处改动（回滚可用版本 ${j.snapshotVersion}）` +
        (j.runId ? `，已起跑 run #${j.runId}` : '')
      onNotify?.(msg)
      setCands(null)
      setCandFor(null)
      void refresh()
    } catch (e) {
      onNotify?.('应用失败：' + String(e))
    } finally {
      setBusy('')
    }
  }

  if (!open) return null

  const o = report?.overall

  return (
    <div className="fixed inset-0 z-40 flex" onClick={onClose}>
      <div
        className="ml-auto h-full w-[560px] max-w-[92vw] flex flex-col fe-drawer fe-scroll-thin overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="fe-drawer__hd sticky top-0 z-10">
          <span className="text-[13px] font-semibold text-white">运行诊断</span>
          <span className="text-[10px] text-gray-500">
            {report ? `${report.total} 条记录 · evidence ${report.fromEvidence} / 历史 ${report.fromHistory}` : ''}
          </span>
          <button
            className="ml-auto text-[11px] text-data hover:underline disabled:opacity-40"
            onClick={() => void refresh()}
            disabled={loading}
          >
            {loading ? '读取中…' : '刷新'}
          </button>
          <button className="fe-drawer__x" onClick={onClose}>
            ×
          </button>
        </div>

        {/* 总览：三个数 + 可信度 */}
        {o && (
          <div className="fe-drawer__hd">
            <div className="flex items-end gap-5">
              {[
                ['完成率', pct(o.crAll), 'all case 为分母'],
                ['门通过率', pct(o.crClean), o.crClean === null ? '没有门样本' : '只算真过过门'],
                ['不可测', pct(o.unscoredRate), '越高分数越不可信'],
              ].map(([label, val, hint]) => (
                <div key={label} className="flex flex-col">
                  <span className="text-[10px] text-gray-500">{label}</span>
                  <span className="mono-num text-[19px] font-semibold text-white leading-tight">{val}</span>
                  <span className="text-[9px] text-gray-600">{hint}</span>
                </div>
              ))}
              <div className="ml-auto flex flex-col items-end">
                <span
                  className={`text-[10px] px-1.5 py-0.5 rounded ${
                    report.trustworthy
                      ? 'bg-green-500/15 text-green-400'
                      : 'bg-red-500/15 text-red-400'
                  }`}
                >
                  {report.trustworthy ? '可下结论' : '分数不可信'}
                </span>
                <span className="text-[9px] text-gray-600 mt-1">
                  {o.passed} 过 / {o.failed} 门不过 / {o.aborted} 中断 · 均 {fmtDur(o.avgMs)}
                </span>
              </div>
            </div>
            {report.note && <p className="text-[10px] text-amber-400/80 mt-2 leading-relaxed">{report.note}</p>}
          </div>
        )}

        {error && <p className="px-4 py-3 text-[11px] text-red-400">{error}</p>}

        {/* 诊断列表 */}
        <div className="px-4 py-3 flex flex-col gap-2">
          {report && (report.diagnoses?.length ?? 0) === 0 && (
            <p className="text-[11px] text-gray-500">没有发现明显问题。跑更多任务后这里会有结论。</p>
          )}
          {/* ?? [] 不是多余的：老接口（和任何返回 null 的实现）会让 .map 抛错白屏 */}
          {(report?.diagnoses ?? []).map((d, i) => (
            <div key={i} className={`rounded-lg p-2.5 ${SEV_STYLE[d.severity] || 'bg-white/[0.03]'}`}>
              <div className="flex items-center gap-2">
                <span className="text-[9px] font-mono text-gray-500">{d.severity}</span>
                <span className="text-[9px] px-1 rounded bg-surface-hover/60 text-gray-400">{d.kind}</span>
                <span className="text-[10px] text-gray-500 truncate">{d.target}</span>
                {d.workflowId ? (
                  <button
                    className="ml-auto shrink-0 text-[10px] text-data hover:underline"
                    onClick={() => void propose(d)}
                    disabled={busy === 'propose'}
                  >
                    生成候选
                  </button>
                ) : (
                  <span className="ml-auto shrink-0 text-[9px] text-gray-600">全局</span>
                )}
              </div>
              <p className="text-[11.5px] text-gray-200 mt-1 leading-relaxed">{d.message}</p>
              {d.metric && <p className="text-[10px] text-gray-500 font-mono mt-1">{d.metric}</p>}
              <p className="text-[10.5px] text-gray-400 mt-1 leading-relaxed">→ {d.suggestion}</p>
            </div>
          ))}
        </div>

        {/* 候选区 */}
        {candFor && (
          <div className="px-4 pb-6">
            <div className="flex items-center gap-2 mb-2">
              <span className="text-[11px] text-gray-400">
                候选（{candFor.target}）
              </span>
              <button
                className="ml-auto text-[10px] text-gray-500 hover:text-white"
                onClick={() => { setCands(null); setCandFor(null) }}
              >
                收起
              </button>
            </div>

            {busy === 'propose' && <p className="text-[11px] text-gray-500">读取当前参数…</p>}
            {candNote && <p className="text-[10.5px] text-amber-400/90 leading-relaxed mb-2">{candNote}</p>}

            {(cands || []).map((c) => (
              <div key={c.id} className="rounded-lg border border-border bg-surface-hover/30 p-2.5 mb-2">
                <div className="flex items-center gap-2">
                  <span className="text-[11.5px] text-white">{c.title}</span>
                  <span className="ml-auto text-[9px] px-1 rounded bg-indigo-500/15 text-indigo-300">
                    {c.confidence}
                  </span>
                </div>

                {c.edits.length > 0 && (
                  <div className="mt-2 flex flex-col gap-1">
                    {c.edits.map((e, k) => (
                      <div key={k} className="text-[10px] font-mono text-gray-400 flex items-center gap-1.5">
                        <span className="text-gray-500">{e.nodeId}</span>
                        <span className="text-gray-600">{e.path.replace(/^data\./, '')}</span>
                        <span className="text-red-400/80">{JSON.stringify(e.from)}</span>
                        <span className="text-gray-600">→</span>
                        <span className="text-green-400">{JSON.stringify(e.to)}</span>
                      </div>
                    ))}
                  </div>
                )}

                <p className="text-[10px] text-gray-500 mt-2 leading-relaxed">依据：{c.basis}</p>
                <p className="text-[10px] text-gray-500 leading-relaxed">预期：{c.expected}</p>
                <p className="text-[10px] text-amber-500/80 leading-relaxed">风险：{c.risk}</p>

                {c.edits.length > 0 && (
                  <div className="flex gap-2 mt-2.5">
                    <button
                      className="px-2 py-1 rounded-md text-[10.5px] bg-surface-hover/60 text-gray-300 hover:bg-white/[0.1] disabled:opacity-40"
                      onClick={() => void trial(c, false)}
                      disabled={!!busy}
                    >
                      只应用（不跑）
                    </button>
                    <button
                      className="px-2 py-1 rounded-md text-[10.5px] bg-indigo-500/85 text-white hover:bg-indigo-500 disabled:opacity-40"
                      onClick={() => void trial(c, true)}
                      disabled={!!busy}
                    >
                      {busy === 'run' ? '起跑中…' : '应用并重跑'}
                    </button>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}

        {/* 底部说明 */}
        <div className="mt-auto px-4 py-3 border-t border-border">
          <p className="text-[9.5px] text-gray-600 leading-relaxed">
            数据源：<span className="font-mono">data/evidence/*.json</span>（带门明细）+ 运行历史（只有状态）。
            改图前会自动快照，随时可回滚；「应用并重跑」会占用显存，请确认没有别的任务在跑。
          </p>
        </div>
      </div>
    </div>
  )
}
