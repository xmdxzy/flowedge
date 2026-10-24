import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render, waitFor } from '@testing-library/react'

import DiagnosticsPanel from './DiagnosticsPanel'

// 诊断面板在最"正常"的情况下最容易崩：没有任何问题时。
//
// 2026-10-23 实测：后端把空诊断序列化成 `null`（Go 的 nil slice），
// 前端写的是 `report?.diagnoses.map(...)` —— `?.` 只保护了 report，
// 没保护 diagnoses，于是"一切正常"时面板直接白屏。
// 那天第一次跑出全通过的证据（padding 3 条、0 条诊断）才暴露出来。
//
// 这组用例同时锁前端（防御 null）与契约（空集合要表达成 []，不是 null）。

function stubFetch(body: unknown) {
  const orig = globalThis.fetch
  globalThis.fetch = (async () => ({ ok: true, status: 200, json: async () => body })) as any
  return () => (globalThis.fetch = orig)
}

const EMPTY_REPORT = {
  total: 3,
  fromEvidence: 3,
  fromHistory: 0,
  overall: {
    workflowId: 0,
    name: '',
    runs: 3,
    passed: 3,
    failed: 0,
    aborted: 0,
    unscored: 0,
    incomplete: 0,
    crAll: 1,
    crClean: 1,
    abortedRate: 0,
    unscoredRate: 0,
    avgMs: 1200,
    p95Ms: 2000,
    hasEvidence: true,
  },
  workflows: [],
  trustworthy: true,
}

test('诊断面板：后端返回 diagnoses=null 时不能白屏', async () => {
  const restore = stubFetch({ report: { ...EMPTY_REPORT, diagnoses: null }, badFiles: 0 })
  try {
    const { container } = render(<DiagnosticsPanel open={true} onClose={() => {}} />)
    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('没有发现明显问题'), '应显示"没问题"而不是崩掉')
    })
  } finally {
    restore()
  }
})

test('诊断面板：diagnoses=[] 与 null 一样走"没问题"分支', async () => {
  const restore = stubFetch({ report: { ...EMPTY_REPORT, diagnoses: [] }, badFiles: 0 })
  try {
    const { container } = render(<DiagnosticsPanel open={true} onClose={() => {}} />)
    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('没有发现明显问题'))
    })
  } finally {
    restore()
  }
})

test('诊断面板：三数齐全，且 crClean 为 null 时显示破折号而非 100%', async () => {
  const restore = stubFetch({
    report: {
      ...EMPTY_REPORT,
      overall: { ...EMPTY_REPORT.overall, passed: 3, failed: 0, crClean: null },
      diagnoses: [],
    },
    badFiles: 0,
  })
  try {
    const { container } = render(<DiagnosticsPanel open={true} onClose={() => {}} />)
    await waitFor(() => {
      const t = container.textContent ?? ''
      assert.ok(t.includes('门通过率'), '要显示门通过率这一项')
      // 没有门样本时必须是「—」，报 100% 会把"没测过"伪装成"全过"
      assert.ok(t.includes('没有门样本'), '要说明为什么没有门通过率')
    })
    assert.ok(!(container.textContent ?? '').includes('门通过率100%'), '不能把 null 显示成 100%')
  } finally {
    restore()
  }
})
