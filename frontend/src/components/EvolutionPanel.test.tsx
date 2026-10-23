import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render, waitFor } from '@testing-library/react'

import EvolutionPanel from './EvolutionPanel'
import { fire } from '../test-utils'

// 自进化面板的渲染契约。
//
// 盯的四件容易做错的事：
//   - 门覆盖率/燃料只摆数字不给结论（人看完不知道该干什么）
//   - 失分排行不按 lossShare 排序（最该修的排最后）
//   - 晋升/保留允许空理由落库（台账退化成日记）
//   - 判定结论不显示配对数值（"promote"两个字没有可信度）

const noop = () => {}

function stubFetch(responder: (url: string, init?: RequestInit) => { ok: boolean; status: number; body: unknown }) {
  const calls: Array<{ url: string; body: any }> = []
  const orig = globalThis.fetch
  globalThis.fetch = (async (url: any, init?: any) => {
    let body: any = undefined
    if (init?.body) {
      try {
        body = JSON.parse(init.body)
      } catch {
        body = init.body
      }
    }
    calls.push({ url: String(url), body })
    const r = responder(String(url), init)
    return { ok: r.ok, status: r.status, json: async () => r.body } as any
  }) as any
  return { calls, restore: () => (globalThis.fetch = orig) }
}

const READINESS = {
  workflows: 12,
  withGate: 3,
  withParams: 8,
  gateCoverage: 0.25,
  gatedWorkflows: [{ id: 113, name: '出片主线', gates: 1, knobs: 6 }],
  verdict: '门覆盖率不到一半：能进化的只有那几条图，其余改动全是盲改',
}

const STATUS_WF = {
  policy: { minPairs: 8, minObjectiveDelta: 0.03, maxTokenRegressionPct: 0, maxLatencyPct: 10, maxCriticalRegressions: 0 },
  fuel: {
    evidence: 20,
    scored: 18,
    unscoredRate: 0.1,
    pairableCases: 9,
    pairableRuns: 18,
    workflows: 1,
    // 注意：categories / blockers 故意**不写** —— Go 侧 omitempty，
    // 空数组时这两个键整个不出现。2026-10-23 真机事故：面板直接
    // fuel.categories.length 炸掉整棵 React 树（选中工作流后白屏）。
    ready: true,
  },
  attribution: [
    { gateCheck: 'loudness_min_db', failed: 4, total: 18, failRate: 0.22, lossShare: 0.31, verdict: 'param_tunable', reason: '响度低于平台基准' },
    { gateCheck: 'consistency_layout', failed: 9, total: 18, failRate: 0.5, lossShare: 0.69, verdict: 'needs_human', reason: '布局漂移，没有标准值可调' },
  ],
  surface: { total: 6, locked: 1, related: 2, knobs: [] },
  next: 'POST /api/evolution/propose 拿候选',
}

const PROMOS = {
  count: 2,
  records: [
    {
      id: 2, workflow_id: 113, action: 'promote', source: 'promote', verdict: 'promote',
      from_version: 3, to_version: 4, pairs: 9, matched_cases: 9,
      gate_score_delta: 0.041, token_delta_pct: -2.5, latency_delta_pct: 1.2, critical_regressions: 0,
      reason: '门分提升 4.1%，9 对 8 胜', actor: '张越', created_at: '2026-10-23T09:00:00Z',
    },
    {
      id: 1, workflow_id: 113, action: 'archive', source: 'decide', verdict: 'keep_baseline',
      from_version: 0, to_version: 0, pairs: 9, matched_cases: 9,
      gate_score_delta: -0.01, token_delta_pct: 0.4, latency_delta_pct: 3.1, critical_regressions: 0,
      reason: 'decide 归档：verdict=keep_baseline', actor: 'engine', created_at: '2026-10-22T09:00:00Z',
    },
  ],
}

// decide 响应可被用例改写（回归 null reasons 用）
let DECIDE_BODY: Record<string, unknown> = {
  decision: { verdict: 'promote', pairs: 9, better: 8, worse: 0, tie: 1, gateScoreDelta: 0.041, tokenDeltaPct: -2.5, latencyDeltaPct: 1.2, criticalRegressions: 0, reasons: ['门分提升超过 3%'] },
  recorded: true,
}

function responder(url: string) {
  if (url.startsWith('/api/workflows')) return { ok: true, status: 200, body: [{ id: 113, name: '出片主线' }] }
  if (url.startsWith('/api/evolution/status?workflowId=')) return { ok: true, status: 200, body: STATUS_WF }
  if (url.startsWith('/api/evolution/status')) return { ok: true, status: 200, body: { readiness: READINESS } }
  if (url.startsWith('/api/evolution/promotions')) return { ok: true, status: 200, body: PROMOS }
  if (url.startsWith('/api/evolution/decide')) return { ok: true, status: 200, body: DECIDE_BODY }
  if (url.startsWith('/api/evolution/propose')) {
    return { ok: true, status: 200, body: { count: 1, candidates: [{ id: 'c1', workflowId: 113, title: '响度归一对齐平台基准', diagnosisKind: 'gate_weak', edits: [{ nodeId: 'pp-1', path: 'data.loudness', from: -20, to: -14, why: 'EBU R128' }], expected: 'loudness_min_db 通过率', risk: '低', confidence: 'high', basis: '平台基准 -14 LUFS' }] } }
  }
  if (url.startsWith('/api/evolution/promote')) return { ok: true, status: 200, body: { recorded: true, applied: false } }
  return { ok: true, status: 200, body: {} }
}

test('不选工作流时给全库门覆盖率（不是空屏）', async () => {
  const { calls, restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('门覆盖率')))
    assert.ok(container.textContent?.includes('25.0%'), '覆盖率应按百分比显示')
    assert.ok(container.textContent?.includes('3/12'), '应显示 接了门的图/总图数')
    // 结论要能指导行动：覆盖率不足时把 verdict 摆出来
    assert.ok(container.textContent?.includes('其余改动全是盲改'))
    assert.ok(calls.some((c) => c.url.startsWith('/api/evolution/status')))
  } finally {
    restore()
  }
})

test('选中工作流后显示燃料瓷砖与失分排行（按 lossShare 排序）', async () => {
  const { restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('可配对 case')))
    assert.ok(container.textContent?.includes('燃料就绪'))

    // 失分排行：lossShare 大的在前（consistency_layout 0.69 > loudness 0.31）
    await waitFor(() => assert.ok(container.textContent?.includes('consistency_layout')))
    const text = container.textContent || ''
    assert.ok(
      text.indexOf('consistency_layout') < text.indexOf('loudness_min_db'),
      '失分应按占总失分比例降序排',
    )
    assert.ok(text.includes('needs_human'), 'verdict 徽章要显示')
  } finally {
    restore()
  }
})

test('晋升表单理由为空时拒绝落库', async () => {
  const { calls, restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('晋升台账')))

    fire(() => {
      const btns = Array.from(container.querySelectorAll('button'))
      btns.find((b) => b.textContent === '晋升…')?.click()
    })
    // 空理由直接点确认
    fire(() => {
      const btns = Array.from(container.querySelectorAll('button'))
      btns.find((b) => b.textContent === '确认晋升')?.click()
    })
    await waitFor(() => assert.ok(container.textContent?.includes('理由必填')))
    assert.ok(
      !calls.some((c) => c.url.startsWith('/api/evolution/promote')),
      '空理由不许发出 promote 请求',
    )
  } finally {
    restore()
  }
})

test('跑配对判定：verdict 与配对数值上屏，record=true 落台账', async () => {
  const { calls, restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('跑配对判定')))

    fire(() => {
      const btns = Array.from(container.querySelectorAll('button'))
      btns.find((b) => b.textContent === '跑配对判定')?.click()
    })
    await waitFor(() => assert.ok(container.textContent?.includes('9 对')))

    const decide = calls.find((c) => c.url.startsWith('/api/evolution/decide'))
    assert.ok(decide, '应调用 decide')
    assert.equal(decide!.body.record, true, '结论落台账默认勾上')
    assert.ok(container.textContent?.includes('8 胜'), '配对胜负要显示')
    assert.ok(container.textContent?.includes('+0.041'), '门分差要显示')
  } finally {
    restore()
  }
})

test('台账按动作渲染标签（晋升/引擎归档），数值快照可见', async () => {
  const { restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('张越')))
    const text = container.textContent || ''
    assert.ok(text.includes('晋升'), 'promote 记录应显示「晋升」')
    assert.ok(text.includes('引擎归档'), 'archive 记录应显示「引擎归档」')
    assert.ok(text.includes('v3 → v4'), '版本跨度要显示')
    assert.ok(text.includes('引擎结论 promote'), '引擎当时的结论要可见')
  } finally {
    restore()
  }
})

test('生成候选：from → to 与依据都要上屏', async () => {
  const { restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    await waitFor(() => assert.ok(container.textContent?.includes('生成候选')))
    fire(() => {
      const btns = Array.from(container.querySelectorAll('button'))
      btns.find((b) => b.textContent === '生成候选')?.click()
    })
    await waitFor(() => assert.ok(container.textContent?.includes('响度归一对齐平台基准')))
    const text = container.textContent || ''
    assert.ok(text.includes('-20 → -14'), '候选必须显示 from → to，不能只给目标值')
    assert.ok(text.includes('平台基准 -14 LUFS'), '依据要可见，方便人驳回')
  } finally {
    restore()
  }
})

// 真机回归（2026-10-23）：Go 的 omitempty / nil slice 让 categories、
// blockers、reasons 整个键消失或为 null。面板按完整形状写就直接白屏 ——
// 选中工作流后整棵 React 树卸载，控制台只有一句 Cannot read 'length'。
// 这条用例钉住「按可能缺失处理」。
test('回归：API 省略空数组（categories/blockers/reasons）不许炸', async () => {
  DECIDE_BODY = {
    decision: { verdict: 'keep_baseline', pairs: 2, better: 0, worse: 2, tie: 0, gateScoreDelta: -0.05, tokenDeltaPct: 0, latencyDeltaPct: 0, criticalRegressions: 0 },
    recorded: true,
  }
  const { restore } = stubFetch(responder)
  try {
    const { container } = render(<EvolutionPanel workflowId={113} onClose={noop} />)
    // 燃料区渲染（categories/blockers 键不存在）
    await waitFor(() => assert.ok(container.textContent?.includes('燃料就绪')))
    assert.ok(container.textContent?.includes('可配对 case'), '燃料瓷砖应渲染')

    // 判定区渲染（decision.reasons 为 null）
    fire(() => {
      const btns = Array.from(container.querySelectorAll('button'))
      btns.find((b) => b.textContent === '跑配对判定')?.click()
    })
    await waitFor(() => assert.ok(container.textContent?.includes('keep_baseline')))
    assert.ok(container.querySelectorAll('button').length > 0, '面板不许白屏（按钮应仍在）')
  } finally {
    restore()
    DECIDE_BODY = {
      decision: { verdict: 'promote', pairs: 9, better: 8, worse: 0, tie: 1, gateScoreDelta: 0.041, tokenDeltaPct: -2.5, latencyDeltaPct: 1.2, criticalRegressions: 0, reasons: ['门分提升超过 3%'] },
      recorded: true,
    }
  }
})
