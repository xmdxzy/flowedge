import { useCallback, useEffect, useMemo, useState } from 'react'

import {
  fetchEvolutionStatus,
  fetchPromotions,
  fetchWorkflowBriefs,
  pressPromotion,
  proposeCandidates,
  runDecide,
  type EvoAttribution,
  type EvoCandidate,
  type EvoDecision,
  type EvoFuel,
  type EvoPromotion,
  type EvoReadiness,
  type EvoStatus,
  type WorkflowBrief,
} from '../lib/evolution'

// 自进化面板：一屏答完「这台引擎的图有没有在变好」。
//
// 设计取舍：
//   - 不做仪表盘。四个区块各回答一个问题，且都带「下一步做什么」：
//     燃料够不够 → 失分漏在哪 → 该改哪 → 改得对不对（判定 + 台账）。
//   - 晋升/保留是显式动作且理由必填：闭环里唯一不可自动重放的决策，
//     不能变成一个顺手点的按钮。表单默认收起，不给「误按晋升」留路。
//   - 所有数值直接来自引擎端点，面板不自己算通过率/均值 ——
//     前端算数就是第二套口径，两套口径必然有一天对不上。

interface Props {
  workflowId?: number
  onClose: () => void
  onNotify?: (msg: string) => void
}

// 状态色全部走主题变量（--fe-*-soft / --fe-*-line 成对定义在 index.css），
// 组件里不写颜色字面量 —— 深浅主题才能自动跟随。
const TONE = {
  ok: { color: 'var(--fe-ok-fg)', background: 'var(--fe-ok-soft)', borderColor: 'var(--fe-ok-line)' },
  warn: { color: 'var(--fe-warn-fg)', background: 'var(--fe-warn-soft)', borderColor: 'var(--fe-warn-line)' },
  bad: { color: 'var(--fe-danger-fg)', background: 'var(--fe-danger-soft)', borderColor: 'var(--fe-danger-line)' },
  info: { color: 'var(--fe-brand-ink)', background: 'var(--fe-brand-soft)', borderColor: 'var(--fe-brand-line)' },
  muted: { color: 'var(--fe-muted)', background: 'transparent', borderColor: 'var(--fe-line)' },
} as const

type Tone = keyof typeof TONE

function Pill({ tone, children }: { tone: Tone; children: React.ReactNode }) {
  return (
    <span className="fe-pill" style={TONE[tone]}>
      {children}
    </span>
  )
}

const VERDICT_TONE: Record<string, Tone> = {
  promote: 'ok',
  healthy: 'ok',
  keep_baseline: 'warn',
  regressed: 'bad',
  insufficient: 'muted',
  inconclusive: 'muted',
  param_tunable: 'info',
  needs_human: 'warn',
  gate_side: 'muted',
  unmeasured: 'bad',
}

const ACTION_LABEL: Record<string, string> = {
  promote: '晋升',
  keep: '保留',
  rollback: '回滚',
  archive: '引擎归档',
}

function pct(x: number): string {
  return `${(x * 100).toFixed(1)}%`
}

function signed(x: number): string {
  return `${x >= 0 ? '+' : ''}${x.toFixed(3)}`
}

export default function EvolutionPanel({ workflowId, onClose, onNotify }: Props) {
  const [workflows, setWorkflows] = useState<WorkflowBrief[]>([])
  const [wfId, setWfId] = useState<number | undefined>(workflowId)
  const [status, setStatus] = useState<EvoStatus | null>(null)
  const [readiness, setReadiness] = useState<EvoReadiness | null>(null)
  const [promos, setPromos] = useState<EvoPromotion[]>([])
  const [candidates, setCandidates] = useState<EvoCandidate[] | null>(null)
  const [decision, setDecision] = useState<EvoDecision | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [note, setNote] = useState('')
  // 结论落台账默认勾上：台账的价值就在于「每次查看都有据可查」，
  // 但要看得见、可取消 —— 隐式落库等于偷偷记账。
  const [record, setRecord] = useState(true)
  const [formOpen, setFormOpen] = useState<'' | 'promote' | 'keep'>('')
  const [form, setForm] = useState({ reason: '', from: '', to: '', actor: '', apply: false })

  const notify = (m: string) => onNotify?.(m)

  const loadWorkflows = useCallback(async () => {
    try {
      setWorkflows(await fetchWorkflowBriefs())
    } catch {
      /* 选择器拉不到不影响主区块；留空让人手输 */
    }
  }, [])

  const loadReadiness = useCallback(async () => {
    try {
      const s = await fetchEvolutionStatus()
      setReadiness(s.readiness ?? null)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  const loadStatus = useCallback(async (id?: number) => {
    if (!id) {
      setStatus(null)
      return
    }
    try {
      setStatus(await fetchEvolutionStatus(id))
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  const loadPromos = useCallback(async (id?: number) => {
    if (!id) {
      setPromos([])
      return
    }
    try {
      const r = await fetchPromotions(id)
      setPromos(r.records || [])
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    void loadWorkflows()
    void loadReadiness()
  }, [loadWorkflows, loadReadiness])

  useEffect(() => {
    setWfId(workflowId)
  }, [workflowId])

  useEffect(() => {
    void loadStatus(wfId)
    void loadPromos(wfId)
  }, [wfId, loadStatus, loadPromos])

  const selectWf = (id: number | undefined) => {
    setWfId(id)
    setCandidates(null)
    setDecision(null)
    setNote('')
  }

  const doPropose = async () => {
    if (!wfId) return
    setBusy('propose')
    setErr('')
    try {
      const r = await proposeCandidates(wfId)
      setCandidates(r.candidates || [])
      setNote(r.count === 0 ? '没有可自动提的候选：要么门全过，要么失分都落在「需要人定」的旋钮上' : '')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const doDecide = async () => {
    if (!wfId) return
    setBusy('decide')
    setErr('')
    try {
      const r = await runDecide(wfId, { record })
      setDecision(r.decision)
      if (r.recorded) {
        await loadPromos(wfId)
        notify('判定结论已归档进晋升台账')
      }
      setNote('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const submitForm = async () => {
    if (!wfId || !formOpen) return
    if (!form.reason.trim()) {
      setErr('理由必填：没有理由的判定不许落库')
      return
    }
    const to = Number(form.to)
    if (formOpen === 'promote' && (!Number.isFinite(to) || to <= 0)) {
      setErr('晋升必须给出目标版本号（toVersion）')
      return
    }
    setBusy('press')
    setErr('')
    try {
      const r = await pressPromotion(wfId, {
        action: formOpen,
        reason: form.reason.trim(),
        fromVersion: form.from ? Number(form.from) : undefined,
        toVersion: formOpen === 'promote' ? to : undefined,
        actor: form.actor.trim() || undefined,
        apply: formOpen === 'promote' ? form.apply : false,
      })
      await loadPromos(wfId)
      setFormOpen('')
      setForm({ reason: '', from: '', to: '', actor: '', apply: false })
      notify(
        formOpen === 'promote'
          ? r.applied
            ? `已晋升到 v${to} 并写成当前图（原图已快照）`
            : '已落台账（dry-run，未写图）'
          : '已记录「保留基线」',
      )
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const fuel: EvoFuel | undefined = status?.fuel
  // 可选数组全部兜底：Go 的 omitempty 会把空 slices 整个省掉（blockers/categories），
  // nil slice 序列化成 null（reasons）。直接 .length/.map 会炸掉整棵 React 树 ——
  // 真机上报过：选中工作流后面板白屏，根因就是这三处。
  const blockers = fuel?.blockers ?? []
  const categories = fuel?.categories ?? []
  const reasons = decision?.reasons ?? []
  const attrs: EvoAttribution[] = useMemo(
    () => (status?.attribution || []).slice().sort((a, b) => b.lossShare - a.lossShare),
    [status],
  )

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[600px] h-full fe-drawer fe-scroll-thin flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="fe-drawer__hd">
          <div className="flex items-center gap-2">
            <span className="fe-drawer__title">自进化</span>
            <span className="fe-drawer__sub">引擎有没有在变好：燃料 → 失分 → 候选 → 判定 → 台账</span>
            <button onClick={onClose} className="fe-drawer__x">
              ✕
            </button>
          </div>
          <div className="mt-2 flex items-center gap-1.5">
            <select
              value={wfId ?? ''}
              onChange={(e) => selectWf(e.target.value ? Number(e.target.value) : undefined)}
              className="px-2 py-0.5 rounded bg-canvas border border-border text-[10px] fe-t1 outline-none focus:border-primary max-w-[220px]"
            >
              <option value="">全库就绪度（不选工作流）</option>
              {workflows.map((w) => (
                <option key={w.id} value={w.id}>
                  #{w.id} {w.name}
                </option>
              ))}
            </select>
            <button
              onClick={() => {
                void loadReadiness()
                void loadStatus(wfId)
                void loadPromos(wfId)
              }}
              className="fe-btn-ghost px-2 py-0.5 rounded text-[10px]"
            >
              刷新
            </button>
          </div>
        </div>

        {err && (
          <div
            className="px-4 py-1.5 text-[10px] border-b"
            style={TONE.bad}
          >
            {err}
            <button onClick={() => setErr('')} className="ml-2 fe-dim hover:fe-t1">
              收起
            </button>
          </div>
        )}

        <div className="flex-1 overflow-y-auto">
          {/* A. 燃料与门覆盖 */}
          <section className="px-4 py-3 border-b border-border">
            <div className="fe-section-label">燃料 · 有没有尺子在产分数</div>
            {readiness && (
              <div className="mt-1.5 flex items-center gap-1.5 flex-wrap">
                <Pill tone={readiness.withGate > 0 ? 'ok' : 'bad'}>
                  门覆盖率 {pct(readiness.gateCoverage)}（{readiness.withGate}/{readiness.workflows} 条图接了门）
                </Pill>
                <span className="text-[10px] fe-dim leading-relaxed">{readiness.verdict}</span>
              </div>
            )}
            {fuel ? (
              <>
                <div className="mt-2 grid grid-cols-4 gap-1.5">
                  <Tile label="已评分" value={String(fuel.scored)} sub={`共 ${fuel.evidence} 条证据`} />
                  <Tile label="unscored" value={pct(fuel.unscoredRate)} warn={fuel.unscoredRate > 0.2} sub="没测到的占比" />
                  <Tile label="可配对 case" value={String(fuel.pairableCases)} sub={`${fuel.pairableRuns} 次运行`} />
                  <Tile label="类别" value={String(categories.length)} sub={categories.join(' / ') || '—'} />
                </div>
                <div className="mt-1.5 flex items-center gap-1.5 flex-wrap">
                  <Pill tone={fuel.ready ? 'ok' : 'warn'}>{fuel.ready ? '燃料就绪' : '燃料不足'}</Pill>
                  {status?.next && <span className="text-[10px] fe-dim">{status.next}</span>}
                </div>
                {blockers.length > 0 && (
                  <ul className="mt-1 space-y-0.5">
                    {blockers.map((b) => (
                      <li key={b} className="text-[10px] fe-dim leading-relaxed">
                        · {b}
                      </li>
                    ))}
                  </ul>
                )}
              </>
            ) : (
              <p className="mt-1.5 text-[10px] fe-dim leading-relaxed">
                选一条工作流才能看它的燃料与失分归因。没有门在产分数的库，任何「优化」都无法验证。
              </p>
            )}
          </section>

          {/* B. 失分排行 */}
          {wfId && (
            <section className="px-4 py-3 border-b border-border">
              <div className="fe-section-label">失分排行 · 按占总失分比例排序</div>
              {status?.surface && (
                <p className="mt-1 text-[10px] fe-dim">
                  可调面 {status.surface.total} 个旋钮（门判据 {status.surface.locked} 个永不自动调 /
                  门层词汇 {status.surface.related} 个可调但要留名）
                </p>
              )}
              {attrs.length === 0 ? (
                <p className="mt-1.5 text-[10px] fe-dim leading-relaxed">
                  没有可归因的失分：要么门全过，要么失败的运行都没测到（先看 unscored 占比）。
                </p>
              ) : (
                <div className="mt-1.5 space-y-1.5">
                  {attrs.slice(0, 8).map((a) => (
                    <div key={`${a.nodeId || ''}:${a.gateCheck}`} className="rounded-lg border border-border bg-canvas/40 px-2.5 py-2">
                      <div className="flex items-center gap-1.5 flex-wrap">
                        <span className="text-[11px] fe-t1 font-medium">{a.gateCheck}</span>
                        <Pill tone={VERDICT_TONE[a.verdict] || 'muted'}>{a.verdict}</Pill>
                        <span className="ml-auto text-[10px] fe-num fe-dim">
                          {a.failed}/{a.total} 失败 · 占失分 {pct(a.lossShare)}
                        </span>
                      </div>
                      <p className="mt-0.5 text-[10px] fe-dim leading-relaxed">{a.reason}</p>
                      {(a.knobs || []).length > 0 && (
                        <p className="mt-0.5 text-[9px] fe-dim2 leading-relaxed">
                          旋钮：
                          {(a.knobs || []).map((k) => (
                            <span key={k.key} className="mr-1.5">
                              <span className="font-mono">{k.key}</span>
                              {k.gateLocked ? '（门判据·锁）' : ''} 现为 {String(k.current)}
                            </span>
                          ))}
                        </p>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </section>
          )}

          {/* C. 候选 */}
          {wfId && (
            <section className="px-4 py-3 border-b border-border">
              <div className="fe-section-label">候选 · 该改哪</div>
              <div className="mt-1.5 flex items-center gap-1.5">
                <button
                  onClick={doPropose}
                  disabled={busy === 'propose'}
                  className="fe-btn-ghost px-2 py-0.5 rounded text-[10px]"
                >
                  {busy === 'propose' ? '生成中…' : '生成候选'}
                </button>
                <span className="text-[10px] fe-dim">只提有行业标准值的产出参数；改门判据的编辑已被引擎摘掉</span>
              </div>
              {note && <p className="mt-1 text-[10px] fe-dim">{note}</p>}
              {candidates?.map((c) => (
                <div key={c.id} className="mt-1.5 rounded-lg border border-border bg-canvas/40 px-2.5 py-2">
                  <div className="flex items-center gap-1.5">
                    <span className="text-[11px] fe-t1 font-medium">{c.title}</span>
                    <Pill tone={c.confidence === 'high' ? 'ok' : c.confidence === 'medium' ? 'info' : 'warn'}>
                      {c.confidence}
                    </Pill>
                  </div>
                  {c.edits.map((ed, i) => (
                    <p key={i} className="mt-0.5 text-[10px] fe-dim font-mono">
                      {ed.nodeId}.{ed.path}：{String(ed.from)} → {String(ed.to)}
                    </p>
                  ))}
                  <p className="mt-0.5 text-[10px] fe-dim leading-relaxed">
                    预期 {c.expected} · 风险 {c.risk} · 依据 {c.basis}
                  </p>
                </div>
              ))}
            </section>
          )}

          {/* D. 判定 */}
          {wfId && (
            <section className="px-4 py-3 border-b border-border">
              <div className="fe-section-label">判定 · 改得对不对</div>
              <div className="mt-1.5 flex items-center gap-1.5 flex-wrap">
                <button
                  onClick={doDecide}
                  disabled={busy === 'decide'}
                  className="fe-btn-primary px-2 py-0.5 rounded text-[10px]"
                >
                  {busy === 'decide' ? '判定中…' : '跑配对判定'}
                </button>
                <label className="flex items-center gap-1 text-[10px] fe-dim cursor-pointer">
                  <input type="checkbox" checked={record} onChange={(e) => setRecord(e.target.checked)} />
                  结论落台账
                </label>
              </div>
              {decision && (
                <div className="mt-1.5 rounded-lg border border-border bg-canvas/40 px-2.5 py-2">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    <Pill tone={VERDICT_TONE[decision.verdict] || 'muted'}>{decision.verdict}</Pill>
                    <span className="text-[10px] fe-num fe-dim">
                      {decision.pairs} 对 · {decision.better} 胜 / {decision.worse} 负 / {decision.tie} 平 ·
                      门分差 {signed(decision.gateScoreDelta)}
                    </span>
                  </div>
                  <p className="mt-0.5 text-[10px] fe-num fe-dim">
                    token {signed(decision.tokenDeltaPct)}% · 耗时 {signed(decision.latencyDeltaPct)}% ·
                    fail 级回退 {decision.criticalRegressions}
                    {decision.scopeReduced ? ' · 验收口径被缩小' : ''}
                  </p>
                  {reasons.map((r, i) => (
                    <p key={i} className="mt-0.5 text-[10px] fe-dim leading-relaxed">
                      · {r}
                    </p>
                  ))}
                  {(decision.hacks || []).length > 0 && (
                    <p className="mt-0.5 text-[10px]" style={{ color: 'var(--fe-danger-fg)' }}>
                      检出改评分标准行为：{(decision.hacks || []).join('；')}
                    </p>
                  )}
                </div>
              )}
            </section>
          )}

          {/* E. 晋升台账 */}
          {wfId && (
            <section className="px-4 py-3">
              <div className="fe-section-label">晋升台账 · 谁在哪天凭什么判定</div>
              <div className="mt-1.5 flex items-center gap-1.5">
                <button
                  onClick={() => setFormOpen(formOpen === 'promote' ? '' : 'promote')}
                  className="fe-btn-ghost px-2 py-0.5 rounded text-[10px]"
                >
                  晋升…
                </button>
                <button
                  onClick={() => setFormOpen(formOpen === 'keep' ? '' : 'keep')}
                  className="fe-btn-ghost px-2 py-0.5 rounded text-[10px]"
                >
                  保留…
                </button>
                <span className="text-[10px] fe-dim">引擎不自动晋升：这一步必须是人按的</span>
              </div>

              {formOpen && (
                <div className="mt-1.5 rounded-lg border border-border bg-canvas/40 px-2.5 py-2 space-y-1.5">
                  <textarea
                    value={form.reason}
                    onChange={(e) => setForm({ ...form, reason: e.target.value })}
                    rows={2}
                    placeholder="凭什么晋升/保留？（必填：门分差多少、几对几、哪个指标动的）"
                    className="w-full px-2 py-1 rounded bg-canvas border border-border text-[10px] fe-t1 outline-none focus:border-primary"
                  />
                  <div className="flex items-center gap-1.5">
                    <label className="text-[9px] fe-dim w-14">从版本</label>
                    <input
                      value={form.from}
                      onChange={(e) => setForm({ ...form, from: e.target.value })}
                      placeholder="可空"
                      className="w-16 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] fe-t1 outline-none focus:border-primary"
                    />
                    <label className="text-[9px] fe-dim w-14">到版本</label>
                    <input
                      value={form.to}
                      onChange={(e) => setForm({ ...form, to: e.target.value })}
                      placeholder={formOpen === 'promote' ? '必填' : '可空'}
                      className="w-16 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] fe-t1 outline-none focus:border-primary"
                    />
                    <label className="text-[9px] fe-dim w-14">署名</label>
                    <input
                      value={form.actor}
                      onChange={(e) => setForm({ ...form, actor: e.target.value })}
                      placeholder="manual"
                      className="flex-1 px-1.5 py-0.5 rounded bg-canvas border border-border text-[10px] fe-t1 outline-none focus:border-primary"
                    />
                  </div>
                  {formOpen === 'promote' && (
                    <label className="flex items-center gap-1 text-[10px] fe-dim cursor-pointer">
                      <input
                        type="checkbox"
                        checked={form.apply}
                        onChange={(e) => setForm({ ...form, apply: e.target.checked })}
                      />
                      同时把该版本写成当前图（会先快照，可回滚）
                    </label>
                  )}
                  <div className="flex items-center gap-1.5">
                    <button
                      onClick={submitForm}
                      disabled={busy === 'press'}
                      className="fe-btn-primary px-2 py-0.5 rounded text-[10px]"
                    >
                      {busy === 'press' ? '落库中…' : formOpen === 'promote' ? '确认晋升' : '确认保留'}
                    </button>
                    <button onClick={() => setFormOpen('')} className="fe-btn-ghost px-2 py-0.5 rounded text-[10px]">
                      取消
                    </button>
                  </div>
                </div>
              )}

              {promos.length === 0 ? (
                <p className="mt-1.5 text-[10px] fe-dim leading-relaxed">
                  还没有记录。跑一次判定（勾上「结论落台账」）或按下晋升/保留，这里就会有据可查。
                </p>
              ) : (
                <div className="mt-1.5 space-y-1.5">
                  {promos.map((p) => (
                    <div key={p.id} className="rounded-lg border border-border bg-canvas/40 px-2.5 py-2">
                      <div className="flex items-center gap-1.5 flex-wrap">
                        <Pill tone={p.action === 'promote' ? 'ok' : p.action === 'rollback' ? 'bad' : p.action === 'keep' ? 'warn' : 'muted'}>
                          {ACTION_LABEL[p.action] || p.action}
                        </Pill>
                        <span className="text-[10px] fe-dim">
                          {p.actor} · {new Date(p.created_at).toLocaleString()} · 来自 {p.source}
                        </span>
                        {(p.from_version > 0 || p.to_version > 0) && (
                          <span className="ml-auto text-[10px] fe-num fe-dim">
                            v{p.from_version || '?'} → v{p.to_version || '现图'}
                          </span>
                        )}
                      </div>
                      <p className="mt-0.5 text-[10px] fe-t2 leading-relaxed">{p.reason}</p>
                      {p.verdict && (
                        <p className="mt-0.5 text-[9px] fe-dim">
                          引擎结论 {p.verdict}
                          {p.pairs > 0 &&
                            ` · ${p.pairs} 对 · 门分差 ${signed(p.gate_score_delta)} · token ${signed(p.token_delta_pct)}%`}
                        </p>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </section>
          )}
        </div>

        <div className="px-4 py-2 border-t border-border text-[9px] fe-dim leading-relaxed">
          闭环：接尺子 → 跑配对 → 归因 → 提变体 → 审计 → 判定 → 晋升（人按）→ 盯回归 → 回滚。
          晋升后请定期看 monitor（退化会给出该滚回的版本）。
        </div>
      </div>
    </div>
  )
}

function Tile({ label, value, sub, warn }: { label: string; value: string; sub?: string; warn?: boolean }) {
  return (
    <div className="rounded-lg border border-border bg-canvas/40 px-2 py-1.5">
      <div className="text-[9px] fe-dim">{label}</div>
      <div
        className="text-[15px] fe-num font-semibold"
        style={{ color: warn ? 'var(--fe-warn-fg)' : 'var(--fe-text)' }}
      >
        {value}
      </div>
      {sub && <div className="text-[9px] fe-dim truncate" title={sub}>{sub}</div>}
    </div>
  )
}
