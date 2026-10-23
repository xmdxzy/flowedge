// 自进化面板客户端。
//
// 与 lib/toolchain.ts 的分工：toolchain 是「机器上有什么工具」，
// 这里是「这台引擎的图有没有在变好」——四个问题一屏答完：
//   ① 燃料够不够（有没有门在产分数、配对样本多少）
//   ② 失分漏在哪（归因到具体门检与可调旋钮）
//   ③ 该改哪（候选，只提有行业标准值的产出参数）
//   ④ 改得对不对（配对判定 + 晋升台账）
//
// 为什么 promote/keep 要从界面发：晋升是闭环里唯一不可自动重放的决策，
// 必须有一个显式的、带理由的入口——「引擎不自动晋升」不是口号，
// 是 UI 上就没有那个按钮。

export interface EvoFuel {
  evidence: number
  scored: number
  unscoredRate: number
  pairableCases: number
  pairableRuns: number
  workflows: number
  // categories / blockers 在 Go 侧是 omitempty：为空时整个键不出现，
  // 面板必须按「可能没有」处理（真机事故：直接 .length 炸掉整棵 React 树）。
  categories?: string[]
  ready: boolean
  blockers?: string[]
}

export interface EvoReadiness {
  workflows: number
  withGate: number
  withParams: number
  gateCoverage: number
  gatedWorkflows: { id: number; name: string; gates: number; knobs: number }[]
  verdict: string
}

export interface EvoKnob {
  nodeId: string
  nodeType: string
  title: string
  key: string
  current: unknown
  kind: string
  options?: string[]
  gateLocked: boolean
  gateRelated: boolean
}

export interface EvoAttribution {
  nodeId?: string
  gateCheck: string
  layer?: string
  failed: number
  total: number
  failRate: number
  lossShare: number
  verdict: string
  reason: string
  knobs?: EvoKnob[]
}

export interface EvoStatus {
  policy: { minPairs: number; minObjectiveDelta: number; maxTokenRegressionPct: number; maxLatencyPct: number; maxCriticalRegressions: number }
  fuel: EvoFuel
  evidenceDir: string
  badFiles: number
  readiness?: EvoReadiness
  attribution?: EvoAttribution[]
  surface?: { total: number; locked: number; related: number; knobs: EvoKnob[] }
  next?: string
}

export interface EvoCandidateEdit {
  nodeId: string
  path: string
  from: unknown
  to: unknown
  why?: string
}

export interface EvoCandidate {
  id: string
  workflowId: number
  title: string
  diagnosisKind: string
  edits: EvoCandidateEdit[]
  expected: string
  risk: string
  confidence: string
  basis: string
}

export interface EvoDecision {
  verdict: string // promote / keep_baseline / inconclusive
  // nil slice 序列化成 null：判 inconclusive 时 Go 侧不填 reasons，
  // 前端收到的是 null 不是 []（同样炸过 .map）。
  reasons?: string[] | null
  pairs: number
  matchedCases: number
  better: number
  worse: number
  tie: number
  gateScoreDelta: number
  tokenDeltaPct: number
  latencyDeltaPct: number
  criticalRegressions: number
  hacks?: string[]
  warnings?: string[]
  scopeReduced?: boolean
}

export interface EvoPromotion {
  id: number
  workflow_id: number
  action: string // promote / keep / rollback / archive
  source: string // promote / decide / monitor / rollback
  verdict?: string
  from_version: number
  to_version: number
  pairs: number
  matched_cases: number
  gate_score_delta: number
  token_delta_pct: number
  latency_delta_pct: number
  critical_regressions: number
  reason: string
  actor: string
  note?: string
  created_at: string
}

async function jfetch<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, init)
  if (!r.ok) {
    let msg = `HTTP ${r.status}`
    try {
      const body = await r.json()
      if (body?.error) msg = body.error
      if (body?.hint) msg += `（${body.hint}）`
    } catch {
      /* 响应体不是 JSON：留 HTTP 码 */
    }
    throw new Error(msg)
  }
  return (await r.json()) as T
}

/** 进化状态。workflowId 不给时返回全库就绪度（门覆盖率）。 */
export function fetchEvolutionStatus(workflowId?: number): Promise<EvoStatus> {
  const q = workflowId ? `?workflowId=${workflowId}` : ''
  return jfetch<EvoStatus>(`/api/evolution/status${q}`)
}

/** 候选：只提有行业标准值的产出参数（改门判据的编辑已被引擎摘掉）。 */
export function proposeCandidates(workflowId: number): Promise<{ candidates: EvoCandidate[]; count: number }> {
  return jfetch(`/api/evolution/propose`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ workflowId }),
  })
}

/** 配对判定。record=true 时把结论归档进晋升台账（action=archive）。 */
export function runDecide(
  workflowId: number,
  opts: { record?: boolean; minPairs?: number } = {},
): Promise<{ decision: EvoDecision; recorded: boolean }> {
  return jfetch(`/api/evolution/decide`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ workflowId, record: opts.record ?? false, minPairs: opts.minPairs ?? 0 }),
  })
}

/** 晋升台账（谁在哪天凭什么判定了哪个版本）。 */
export function fetchPromotions(workflowId: number): Promise<{ count: number; records: EvoPromotion[]; latest?: EvoPromotion }> {
  return jfetch(`/api/evolution/promotions?workflowId=${workflowId}`)
}

/** 人按下晋升或保留。promote 且 apply=true 才会写图（先快照，可回滚）。 */
export function pressPromotion(
  workflowId: number,
  body: {
    action: 'promote' | 'keep'
    reason: string
    fromVersion?: number
    toVersion?: number
    actor?: string
    note?: string
    apply?: boolean
  },
): Promise<{ recorded: boolean; applied?: boolean }> {
  return jfetch(`/api/evolution/promote`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ workflowId, ...body }),
  })
}

/** 保存的工作流清单（面板顶部的选择器用）。 */
export interface WorkflowBrief {
  id: number
  name: string
}

export function fetchWorkflowBriefs(): Promise<WorkflowBrief[]> {
  return jfetch<WorkflowBrief[]>(`/api/workflows`)
}
