import { useEffect, useMemo, useState } from 'react'

import { errorText, preflight, produce, startAndWaitDep } from '../api/client'
import type { DepRequirement, Pipeline, PipelineField, PreflightResult } from '../api/types'
import { toast } from '../components/Toast'
import {
  buildParams,
  describeBlockers,
  initialForm,
  type FieldValue,
  type FormValues,
} from '../lib/fields'
import { formatEstSeconds } from '../lib/format'

// 出片页：选一条固定流水线 + 填字段 → 体检 → 提交。
//
// 这里没有画布、没有节点、没有连线：图由服务端按 data/pipelines.json 组装。

const GROUP_META: { kind: string; label: string }[] = [
  { kind: 'image', label: '图像' },
  { kind: 'video', label: '视频' },
  { kind: 'audio', label: '音频' },
]
const GROUP_ICON: Record<string, string> = { image: '🖼', video: '🎬', audio: '🔊' }

interface MissingGroup {
  dep: string
  label: string
  hint: string
  fix: string
  fixable: boolean
  fixDep: string
  nodes: number
}

/** 同一个依赖可能被多个节点同时需要：合并成一行，避免同一句话列三遍 */
function groupMissing(items: DepRequirement[]): MissingGroup[] {
  const map = new Map<string, MissingGroup>()
  for (const item of items) {
    const prev = map.get(item.dep)
    if (prev) {
      prev.nodes += 1
      if (!prev.fix && item.fix) prev.fix = item.fix
      continue
    }
    map.set(item.dep, {
      dep: item.dep,
      label: item.label,
      hint: item.hint,
      fix: item.fix,
      fixable: item.fixable,
      fixDep: item.fix_dep,
      nodes: 1,
    })
  }
  return [...map.values()]
}

interface Props {
  pipelines: Pipeline[]
  loading: boolean
  loadError: string
  onReload: () => void
  onProduced: (runId: number) => void
}

export default function ProducePage({ pipelines, loading, loadError, onReload, onProduced }: Props) {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [form, setForm] = useState<FormValues>({})
  const [touched, setTouched] = useState<Record<string, boolean>>({})
  const [busy, setBusy] = useState(false)
  const [preflightResult, setPreflightResult] = useState<PreflightResult | null>(null)
  const [preflightError, setPreflightError] = useState('')
  const [lastParams, setLastParams] = useState<Record<string, unknown> | null>(null)
  const [fixingDep, setFixingDep] = useState<string | null>(null)

  // 目录到了就默认选中第一条：进页面立刻能填，不用先点一下卡片
  useEffect(() => {
    if (pipelines.length === 0) {
      setSelectedId(null)
      return
    }
    if (!selectedId || !pipelines.some((p) => p.id === selectedId)) {
      setSelectedId(pipelines[0].id)
    }
  }, [pipelines, selectedId])

  const selected = useMemo(
    () => pipelines.find((p) => p.id === selectedId) ?? null,
    [pipelines, selectedId],
  )

  // 换流水线 = 换表单：预填 default，清掉上一条的体检结果
  useEffect(() => {
    if (!selected) {
      setForm({})
      setTouched({})
      return
    }
    setForm(initialForm(selected.fields))
    setTouched({})
    setPreflightResult(null)
    setPreflightError('')
    setLastParams(null)
  }, [selected])

  const groups = useMemo(() => {
    const byKind = new Map<string, Pipeline[]>()
    for (const p of pipelines) {
      const kind = p.output || 'other'
      const list = byKind.get(kind) ?? []
      list.push(p)
      byKind.set(kind, list)
    }
    const known = GROUP_META.filter((g) => byKind.has(g.kind)).map((g) => ({
      ...g,
      items: byKind.get(g.kind) ?? [],
    }))
    const extra = [...byKind.keys()]
      .filter((k) => !GROUP_META.some((g) => g.kind === k))
      .map((k) => ({ kind: k, label: k, items: byKind.get(k) ?? [] }))
    return [...known, ...extra]
  }, [pipelines])

  // 实时校验：按钮是不是可点、缺什么，都从这一份算出来（不另存一份 errors 状态）
  const blockers = useMemo(
    () => (selected ? buildParams(selected.fields, form) : { params: {}, errors: [], missing: [] }),
    [selected, form],
  )

  const missingGroups = useMemo(
    () => groupMissing(preflightResult?.missing ?? []),
    [preflightResult],
  )

  const setValue = (key: string, value: FieldValue) => {
    setForm((prev) => ({ ...prev, [key]: value }))
    setTouched((prev) => ({ ...prev, [key]: true }))
  }

  const checkOnly = async (pipelineId: string, params: Record<string, unknown>) => {
    const result = await preflight(pipelineId, params)
    setPreflightResult(result)
    return result
  }

  const handleSubmit = async () => {
    if (!selected || busy) return
    setTouched(Object.fromEntries(selected.fields.map((f) => [f.key, true])))
    if (blockers.errors.length > 0) {
      toast(describeBlockers(blockers.errors), 'error')
      return
    }

    setBusy(true)
    setPreflightError('')
    setPreflightResult(null)
    setLastParams(blockers.params)
    try {
      const checked = await checkOnly(selected.id, blockers.params)
      // 体检不过：把 missing 逐条摆出来，绝不提交
      if (!checked.ok) return
      const accepted = await produce(selected.id, blockers.params)
      toast(`已提交出片 #${accepted.run_id}`, 'success')
      onProduced(accepted.run_id)
    } catch (err) {
      setPreflightError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const recheck = async () => {
    if (!selected || !lastParams || busy) return
    setBusy(true)
    setPreflightError('')
    try {
      const checked = await checkOnly(selected.id, lastParams)
      if (checked.ok) toast('依赖已就绪，可以出片', 'success')
    } catch (err) {
      setPreflightError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const handleFix = async (group: MissingGroup) => {
    if (!group.fixable || !group.fixDep || fixingDep) return
    setFixingDep(group.fixDep)
    try {
      await startAndWaitDep(group.fixDep)
      toast(`${group.dep} 已就绪，重新体检`, 'success')
      await recheck()
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setFixingDep(null)
    }
  }

  return (
    <div className="page page--produce">
      <aside className="pane pane--list">
        <div className="pane__hd">
          <h2 className="pane__title">流水线</h2>
          <button className="btn btn--ghost btn--sm" onClick={onReload} disabled={busy}>
            重新读取
          </button>
        </div>
        <div className="pane__body scroll">
          {loadError && (
            <div className="notice notice--error" role="alert">
              <p className="notice__title">读不到流水线目录</p>
              <pre className="notice__raw mono">{loadError}</pre>
            </div>
          )}
          {!loadError && loading && pipelines.length === 0 && (
            <p className="empty">读取流水线目录…</p>
          )}
          {!loadError && !loading && pipelines.length === 0 && (
            <p className="empty">还没有流水线。检查 data/pipelines.json。</p>
          )}
          {groups.map((group) => (
            <section key={group.kind} className="pipe-group">
              <h3 className="pipe-group__title">
                <span aria-hidden="true">{GROUP_ICON[group.kind] ?? '▸'}</span>
                {group.label}
                <span className="muted2">{group.items.length}</span>
              </h3>
              <div className="pipe-grid">
                {group.items.map((p) => (
                  <button
                    key={p.id}
                    className={`pipe-card ${p.id === selectedId ? 'is-active' : ''}`}
                    onClick={() => setSelectedId(p.id)}
                  >
                    <span className="pipe-card__name">{p.name}</span>
                    <span className="pipe-card__summary">{p.summary}</span>
                    <span className="pipe-card__meta">
                      <span className="chip chip--quiet">{formatEstSeconds(p.est_seconds)}</span>
                      <span className="mono muted2">{p.id}</span>
                    </span>
                  </button>
                ))}
              </div>
            </section>
          ))}
        </div>
      </aside>

      <section className="pane pane--form">
        {!selected ? (
          <p className="empty">先在左边选一条流水线。</p>
        ) : (
          <>
            <div className="pane__hd">
              <div className="pane__hd-main">
                <h2 className="pane__title">{selected.name}</h2>
                <p className="muted">{selected.summary}</p>
              </div>
              <span className="chip chip--quiet">{formatEstSeconds(selected.est_seconds)}</span>
            </div>

            <div className="pane__body scroll">
              <div className="form">
                {selected.fields.length === 0 && (
                  <p className="muted">这条流水线不需要填字段，直接出片。</p>
                )}
                {selected.fields.map((field) => (
                  <FieldRow
                    key={field.key}
                    field={field}
                    value={form[field.key] ?? ''}
                    error={
                      touched[field.key]
                        ? blockers.errors.find((e) => e.key === field.key)?.message
                        : undefined
                    }
                    onChange={(v) => setValue(field.key, v)}
                  />
                ))}
              </div>

              {preflightError && (
                <div className="notice notice--error" role="alert">
                  <p className="notice__title">请求失败</p>
                  <pre className="notice__raw mono">{preflightError}</pre>
                </div>
              )}

              {preflightResult?.ok && (
                <div className="notice notice--ok" role="status">
                  依赖已就绪，可以出片。
                </div>
              )}

              {preflightResult && !preflightResult.ok && (
                <section className="panel panel--warn">
                  <h3 className="panel__title">依赖没就绪，没有提交出片</h3>
                  <div className="issues">
                    {missingGroups.map((group) => (
                      <div key={group.dep} className="issue">
                        <div className="issue__hd">
                          <span className="issue__label">{group.label}</span>
                          <span className="mono muted2">{group.dep}</span>
                          {group.nodes > 1 && <span className="muted2">{group.nodes} 个节点</span>}
                        </div>
                        {group.fix && <p className="issue__fix">{group.fix}</p>}
                        {group.hint && <p className="issue__hint">{group.hint}</p>}
                        {group.fixable && group.fixDep && (
                          <button
                            className="btn btn--primary btn--sm"
                            disabled={fixingDep !== null}
                            onClick={() => void handleFix(group)}
                          >
                            {fixingDep === group.fixDep ? '启动中…' : '一键启动'}
                          </button>
                        )}
                      </div>
                    ))}
                  </div>
                  <button className="btn btn--ghost btn--sm" onClick={() => void recheck()} disabled={busy}>
                    重新体检
                  </button>
                </section>
              )}
            </div>

            <div className="pane__foot">
              <button
                className="btn btn--primary"
                disabled={busy || blockers.errors.length > 0}
                onClick={() => void handleSubmit()}
              >
                {busy ? '体检 / 提交中…' : '出片'}
              </button>
              {blockers.errors.length > 0 ? (
                <span className="muted">{describeBlockers(blockers.errors)}</span>
              ) : (
                <span className="muted2">先体检依赖，通过后才提交</span>
              )}
            </div>
          </>
        )}
      </section>
    </div>
  )
}

interface FieldRowProps {
  field: PipelineField
  value: FieldValue
  error?: string
  onChange: (value: FieldValue) => void
}

function FieldRow({ field, value, error, onChange }: FieldRowProps) {
  const id = `field-${field.key}`

  return (
    <div className={`field ${error ? 'has-error' : ''}`}>
      <label className="field__label" htmlFor={id}>
        <span>{field.label}</span>
        {field.required && <span className="field__req">必填</span>}
      </label>

      {field.type === 'textarea' && (
        <textarea
          id={id}
          className="input textarea"
          rows={3}
          value={String(value ?? '')}
          placeholder={field.hint}
          onChange={(e) => onChange(e.target.value)}
        />
      )}

      {(field.type === 'text' || field.type === 'path') && (
        <input
          id={id}
          type="text"
          className={`input ${field.type === 'path' ? 'mono' : ''}`}
          value={String(value ?? '')}
          placeholder={field.type === 'path' ? '绝对路径，或 data/ 下的相对路径' : ''}
          onChange={(e) => onChange(e.target.value)}
        />
      )}

      {(field.type === 'int' || field.type === 'float') && (
        <input
          id={id}
          type="number"
          className="input input--num mono"
          value={typeof value === 'number' ? String(value) : String(value ?? '')}
          min={field.min}
          max={field.max}
          step={field.type === 'int' ? 1 : 'any'}
          onChange={(e) => onChange(e.target.value)}
        />
      )}

      {field.type === 'bool' && (
        <button
          type="button"
          id={id}
          role="switch"
          aria-checked={value === true}
          className={`switch ${value === true ? 'is-on' : ''}`}
          onClick={() => onChange(value !== true)}
        >
          <span className="switch__dot" aria-hidden="true" />
          <span className="switch__text">{value === true ? '开' : '关'}</span>
        </button>
      )}

      {field.type === 'enum' && (field.options ?? []).length <= 4 && (
        <div className="segmented" role="group" aria-label={field.label}>
          {(field.options ?? []).map((opt) => (
            <button
              key={opt}
              type="button"
              className={`seg ${value === opt ? 'is-on' : ''}`}
              aria-pressed={value === opt}
              onClick={() => onChange(opt)}
            >
              {opt}
            </button>
          ))}
        </div>
      )}

      {field.type === 'enum' && (field.options ?? []).length > 4 && (
        <select
          id={id}
          className="input select"
          value={String(value ?? '')}
          onChange={(e) => onChange(e.target.value)}
        >
          {(field.options ?? []).map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      )}

      {field.hint && field.type !== 'textarea' && <p className="field__hint">{field.hint}</p>}
      {error && <p className="field__err">{error}</p>}
    </div>
  )
}
