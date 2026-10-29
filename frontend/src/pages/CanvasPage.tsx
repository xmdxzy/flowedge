import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  addEdge,
  Background,
  Controls,
  Handle,
  Position,
  ReactFlow,
  useEdgesState,
  useNodesState,
} from '@xyflow/react'
import type { Connection, Edge, Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'

import { errorText } from '../api/client'
import {
  deleteWorkflow,
  dryRunGraph,
  fetchNodeCatalog,
  fetchTemplates,
  fetchWorkflow,
  listWorkflows,
  runWorkflow,
  saveWorkflow,
  validateGraph,
} from '../api/graph'
import type {
  CatalogNode,
  DryRunResult,
  GraphEdge,
  GraphNode,
  GraphTemplate,
  PortDef,
  WorkflowSummary,
} from '../api/graphTypes'
import { toast } from '../components/Toast'
import {
  catalogMap,
  canConnect,
  connectRejection,
  costliestLines,
  defaultDataFor,
  draftFromTemplate,
  formatEstimate,
  humanDuration,
  issueIndex,
  nextNodeId,
  summarizeReport,
} from '../lib/graph'

// 画布：用户自定义工作流的编辑器。
//
// 与出片台的关系（docs/画布与流水线统一-20261029.md）：
// 两边是**同一份数据**（workflows 表）的两个视图。模板来自 pipelines.json 的反向组装，
// 所以打开画布永远不是"一片空白"，而是"一条已经能跑的图，你在此基础上改"。
//
// 三条不做：不在这里重做运行看板（跳运行页）、不在前端判断节点能否保存
// （后端是唯一裁判，这里只做即时提示）、不硬编码任何节点类型/字段（全部来自 catalog）。

type FlowNodeData = {
  nodeType: string
  label: string
  inPorts: PortDef[]
  outPorts: PortDef[]
  errors: string[]
  warns: string[]
  values: Record<string, unknown>
} & Record<string, unknown>

const NODE_TYPE = 'fnode'

function FlowNodeCard({ data, selected }: { data: FlowNodeData; selected?: boolean }) {
  const bad = (data.errors?.length ?? 0) > 0
  return (
    <div className={`cv-node ${bad ? 'cv-node--err' : ''} ${selected ? 'cv-node--sel' : ''}`}>
      <div className="cv-node__hd">
        <span className="cv-node__label">{data.label}</span>
        <span className="cv-node__type mono">{data.nodeType}</span>
      </div>
      {bad && <div className="cv-node__badge cv-node__badge--err">{data.errors.length} 个错误</div>}
      {!bad && (data.warns?.length ?? 0) > 0 && (
        <div className="cv-node__badge cv-node__badge--warn">{data.warns.length} 个提醒</div>
      )}
      {(data.inPorts ?? []).map((p, i) => (
        <Handle
          key={`in-${p.key}`}
          id={p.key}
          type="target"
          position={Position.Left}
          className={`cv-port cv-port--${p.kind} ${p.required ? 'cv-port--req' : ''}`}
          style={{ top: 24 + i * 18 }}
          title={`输入 ${p.key}（${p.kind}${p.required ? '，必填' : ''}）`}
        />
      ))}
      {(data.outPorts ?? []).map((p, i) => (
        <Handle
          key={`out-${p.key}`}
          id={p.key}
          type="source"
          position={Position.Right}
          className={`cv-port cv-port--${p.kind}`}
          style={{ top: 24 + i * 18 }}
          title={`输出 ${p.key}（${p.kind}）`}
        />
      ))}
    </div>
  )
}

const nodeTypes = { [NODE_TYPE]: FlowNodeCard }

export default function CanvasPage({ onRan }: { onRan?: (runId: number) => void } = {}) {
  const [catalog, setCatalog] = useState<CatalogNode[]>([])
  const [templates, setTemplates] = useState<GraphTemplate[]>([])
  const [saved, setSaved] = useState<WorkflowSummary[]>([])
  const [name, setName] = useState('未命名工作流')
  const [workflowId, setWorkflowId] = useState<number | undefined>(undefined)
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [bootError, setBootError] = useState('')
  const [selection, setSelection] = useState<string | null>(null)
  const [result, setResult] = useState<DryRunResult | null>(null)
  const [newType, setNewType] = useState('')
  const [qc, setQc] = useState(false)
  const flowRef = useRef<HTMLDivElement | null>(null)

  const [nodes, setNodes, onNodesChange] = useNodesState<Node>([])
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([])

  const cat = useMemo(() => catalogMap(catalog), [catalog])

  // ---- 载入：目录 / 模板 / 已存图 ----
  const refreshSaved = useCallback(async () => {
    try {
      const res = await listWorkflows()
      setSaved(res.items)
    } catch (err) {
      toast(`读取工作流列表失败：${errorText(err)}`, 'error')
    }
  }, [])

  useEffect(() => {
    let alive = true
    ;(async () => {
      try {
        const [c, t] = await Promise.all([fetchNodeCatalog(), fetchTemplates()])
        if (!alive) return
        setCatalog(c)
        setTemplates(t)
        if (c.length > 0) setNewType(c[0].type)
        // 首次进入：默认载入第一条模板 —— 空画布是 v1 的头号败因
        const first = t.find((x) => (x.nodes ?? []).length > 0)
        if (first) applyTemplate(first, c)
        setBootError('')
      } catch (err) {
        if (alive) setBootError(errorText(err))
      }
    })()
    void refreshSaved()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshSaved])

  const toFlowNodes = useCallback(
    (raw: GraphNode[], issues: ReturnType<typeof issueIndex>): Node[] =>
      raw.map((n, i) => {
        const c = cat.get(n.type)
        return {
          id: n.id,
          type: NODE_TYPE,
          position: n.position ?? { x: 80 + (i % 4) * 260, y: 80 + Math.floor(i / 4) * 150 },
          data: {
            nodeType: n.type,
            label: c?.label ?? n.type,
            inPorts: c?.in ?? [],
            outPorts: c?.out ?? [],
            errors: issues.byNode.get(n.id)?.errors ?? [],
            warns: issues.byNode.get(n.id)?.warns ?? [],
            values: n.data ?? {},
          } satisfies FlowNodeData,
        } as Node
      }),
    [cat],
  )

  const toFlowEdges = useCallback(
    (raw: GraphEdge[]): Edge[] =>
      raw.map((e) => ({
        id: e.id,
        source: e.source,
        target: e.target,
        sourceHandle: e.sourceHandle ?? undefined,
        targetHandle: e.targetHandle ?? undefined,
      })),
    [],
  )

  const applyTemplate = useCallback(
    (tpl: GraphTemplate, catOverride?: CatalogNode[]) => {
      const map = catalogMap(catOverride ?? catalog)
      const draft = draftFromTemplate(tpl)
      const rawIssues = tpl.report
        ? issueIndex(tpl.report)
        : { byNode: new Map(), byEdge: new Map() }
      setNodes(
        draft.nodes.map((n, i) => ({
          id: n.id,
          type: NODE_TYPE,
          position: n.position ?? { x: 80 + (i % 4) * 260, y: 80 + Math.floor(i / 4) * 150 },
          data: {
            nodeType: n.type,
            label: map.get(n.type)?.label ?? n.type,
            inPorts: map.get(n.type)?.in ?? [],
            outPorts: map.get(n.type)?.out ?? [],
            errors: rawIssues.byNode.get(n.id)?.errors ?? [],
            warns: rawIssues.byNode.get(n.id)?.warns ?? [],
            values: n.data ?? {},
          } satisfies FlowNodeData,
        })),
      )
      setEdges(toFlowEdges(draft.edges))
      setName(tpl.name || tpl.pipeline_id)
      setWorkflowId(undefined)
      setDirty(false)
      setSelection(null)
      setResult(null)
    },
    [catalog, setEdges, setNodes, toFlowEdges],
  )

  const openSaved = useCallback(
    async (id: number) => {
      try {
        const wf = await fetchWorkflow(id)
        const issues = issueIndex(wf.report)
        setNodes(toFlowNodes(wf.nodes, issues))
        setEdges(toFlowEdges(wf.edges))
        setName(wf.name)
        setWorkflowId(wf.id)
        setDirty(false)
        setSelection(null)
        setResult(null)
      } catch (err) {
        toast(`打开失败：${errorText(err)}`, 'error')
      }
    },
    [setEdges, setNodes, toFlowEdges, toFlowNodes],
  )

  // ---- 编辑 ----
  const onConnect = useCallback(
    (conn: Connection) => {
      const src = nodes.find((n) => n.id === conn.source)
      const dst = nodes.find((n) => n.id === conn.target)
      const srcType = (src?.data as FlowNodeData | undefined)?.nodeType
      const dstType = (dst?.data as FlowNodeData | undefined)?.nodeType
      if (!srcType || !dstType) return
      if (!canConnect(cat, srcType, dstType)) {
        toast(connectRejection(cat, srcType, dstType), 'error')
        return
      }
      setEdges((eds) =>
        addEdge({ ...conn, id: `${conn.source}->${conn.target}-${Date.now()}` }, eds),
      )
      setDirty(true)
    },
    [cat, nodes, setEdges],
  )

  const addNode = useCallback(() => {
    const c = cat.get(newType)
    if (!c) return
    const id = nextNodeId(newType, nodes)
    const maxY = nodes.reduce((m, n) => Math.max(m, n.position.y), 40)
    setNodes((ns) =>
      ns.concat({
        id,
        type: NODE_TYPE,
        position: { x: 120, y: maxY + 120 },
        data: {
          nodeType: newType,
          label: c.label,
          inPorts: c.in ?? [],
          outPorts: c.out ?? [],
          errors: [],
          warns: [],
          values: defaultDataFor(c),
        } satisfies FlowNodeData,
      } as Node),
    )
    setDirty(true)
    setSelection(id)
  }, [cat, newType, nodes, setNodes])

  const setField = useCallback(
    (nodeId: string, key: string, value: unknown) => {
      setNodes((ns) =>
        ns.map((n) =>
          n.id === nodeId
            ? {
                ...n,
                data: {
                  ...n.data,
                  values: { ...((n.data as FlowNodeData).values ?? {}), [key]: value },
                },
              }
            : n,
        ),
      )
      setDirty(true)
    },
    [setNodes],
  )

  const removeNode = useCallback(
    (nodeId: string) => {
      setNodes((ns) => ns.filter((n) => n.id !== nodeId))
      setEdges((es) => es.filter((e) => e.source !== nodeId && e.target !== nodeId))
      setSelection(null)
      setDirty(true)
    },
    [setEdges, setNodes],
  )

  // ---- 与服务端交互 ----
  const serverNodes = useCallback(
    (): GraphNode[] =>
      nodes.map((n) => {
        const d = n.data as FlowNodeData
        return { id: n.id, type: d.nodeType, position: n.position, data: d.values ?? {} }
      }),
    [nodes],
  )

  const serverEdges = useCallback(
    (): GraphEdge[] =>
      edges.map((e) => ({
        id: e.id,
        source: e.source,
        target: e.target,
        sourceHandle: e.sourceHandle ?? undefined,
        targetHandle: e.targetHandle ?? undefined,
      })),
    [edges],
  )

  const applyReport = useCallback(
    (report: Awaited<ReturnType<typeof validateGraph>>) => {
      const issues = issueIndex(report)
      setNodes((ns) =>
        ns.map((n) => ({
          ...n,
          data: {
            ...n.data,
            errors: issues.byNode.get(n.id)?.errors ?? [],
            warns: issues.byNode.get(n.id)?.warns ?? [],
          },
        })),
      )
    },
    [setNodes],
  )

  const runDryRun = useCallback(async () => {
    setBusy(true)
    try {
      const res = await dryRunGraph(serverNodes(), serverEdges())
      setResult(res)
      applyReport(res.report)
      if (res.run_ready) toast('干跑通过：结构与依赖都齐了（没提交任何任务）', 'success')
      else toast(`干跑发现问题：${summarizeReport(res.report)}`, 'error')
    } catch (err) {
      toast(`干跑失败：${errorText(err)}`, 'error')
    } finally {
      setBusy(false)
    }
  }, [applyReport, serverEdges, serverNodes])

  const doSave = useCallback(async () => {
    setBusy(true)
    try {
      const rep = await validateGraph(serverNodes(), serverEdges())
      applyReport(rep)
      if (!rep.ok) {
        toast(`图有问题，先修：${summarizeReport(rep)}`, 'error')
        return
      }
      const req = { name: name.trim() || '未命名工作流', nodes: serverNodes(), edges: serverEdges() }
      const res = workflowId ? await saveWorkflow({ ...req, id: workflowId }) : await saveWorkflow(req)
      setWorkflowId(res.id)
      setDirty(false)
      await refreshSaved()
      toast(`已保存：${res.name}（#${res.id}）`, 'success')
    } catch (err) {
      toast(`保存失败：${errorText(err)}`, 'error')
    } finally {
      setBusy(false)
    }
  }, [applyReport, name, refreshSaved, serverEdges, serverNodes, workflowId])

  const doRun = useCallback(async () => {
    if (!workflowId) {
      toast('先保存，再运行（运行的是库里的那张图，不是屏幕上的草稿）', 'error')
      return
    }
    setBusy(true)
    try {
      const res = await runWorkflow(workflowId, { qc, qcRounds: 3 })
      toast(`已入队：run #${res.run_id}${qc ? '（质检 3 轮，跑完择优）' : ''}`, 'success')
      onRan?.(res.run_id)
    } catch (err) {
      toast(`运行失败：${errorText(err)}`, 'error')
    } finally {
      setBusy(false)
    }
  }, [onRan, qc, workflowId])
  const doDelete = useCallback(async () => {
    if (!workflowId) return
    setBusy(true)
    try {
      await deleteWorkflow(workflowId)
      toast('已删除', 'success')
      setWorkflowId(undefined)
      setDirty(false)
      await refreshSaved()
    } catch (err) {
      toast(`删除失败：${errorText(err)}`, 'error')
    } finally {
      setBusy(false)
    }
  }, [refreshSaved, workflowId])

  const selectedNode = nodes.find((n) => n.id === selection) ?? null
  const selectedData = (selectedNode?.data as FlowNodeData | undefined) ?? null
  const selectedDef = selectedData ? cat.get(selectedData.nodeType) : undefined
  const est = result ? formatEstimate(result.estimate) : null

  return (
    <div className="cv-layout">
      <aside className="cv-side">
        <div className="panel">
          <div className="panel__title">从模板新建</div>
          <div className="cv-list">
            {templates.length === 0 && <div className="muted">读不到模板（检查 /api/workflows/templates）</div>}
            {templates.map((t) => (
              <button
                key={t.pipeline_id}
                className="cv-item"
                onClick={() => applyTemplate(t)}
                title={t.summary || t.name}
              >
                <span className="cv-item__name">{t.name || t.pipeline_id}</span>
                <span className="cv-item__meta mono">
                  {(t.nodes ?? []).length} 节点
                  {t.est_seconds ? ` · ${humanDuration(t.est_seconds)}` : ''}
                </span>
              </button>
            ))}
          </div>
        </div>

        <div className="panel">
          <div className="panel__title">已保存的工作流</div>
          <div className="cv-list">
            {saved.length === 0 && <div className="muted">还没有保存过自定义图</div>}
            {saved.map((w) => (
              <button
                key={w.id}
                className="cv-item"
                onClick={() => void openSaved(w.id)}
                title={`${w.origin} · 更新于 ${w.updated_at}`}
              >
                <span className="cv-item__name">
                  #{w.id} {w.name}
                </span>
                <span className="cv-item__meta mono">
                  {w.node_count} 节点 · {w.edge_count} 线
                </span>
              </button>
            ))}
          </div>
        </div>

        <div className="panel">
          <div className="panel__title">加节点</div>
          <div className="cv-add">
            <select className="select" value={newType} onChange={(e) => setNewType(e.target.value)}>
              {catalog.map((c) => (
                <option key={c.type} value={c.type}>
                  {c.label}（{c.type}）
                </option>
              ))}
            </select>
            <button className="btn btn--ghost btn--sm" onClick={addNode} disabled={!newType}>
              +
            </button>
          </div>
        </div>
      </aside>

      <section className="cv-main">
        <div className="cv-bar">
          <input
            className="input cv-bar__name"
            value={name}
            aria-label="工作流名"
            onChange={(e) => {
              setName(e.target.value)
              setDirty(true)
            }}
          />
          {dirty && <span className="chip chip--filter">未保存</span>}
          <span className="muted mono">
            {nodes.length} 节点 / {edges.length} 线
          </span>
          <div className="cv-bar__right">
            <button className="btn btn--ghost btn--sm" onClick={() => void runDryRun()} disabled={busy}>
              干跑（不提交）
            </button>
            <button className="btn btn--primary btn--sm" onClick={() => void doSave()} disabled={busy}>
              保存
            </button>
            {workflowId && (
              <>
              <label className="cv-qc" title="质检：同一张图跑最多 3 轮，按门的结果择优">
                <input type="checkbox" checked={qc} onChange={(e) => setQc(e.target.checked)} />
                质检
              </label>
              <button className="btn btn--ghost btn--sm" onClick={() => void doRun()} disabled={busy} title="运行库里的这张图">
                运行
              </button>
              </>
            )}
            {workflowId && (
              <button className="btn btn--danger btn--sm" onClick={() => void doDelete()} disabled={busy}>
                删除
              </button>
            )}
          </div>
        </div>

        {bootError && <div className="notice notice--error">{bootError}</div>}

        <div className="cv-flow" ref={flowRef}>
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            onNodesChange={(c) => {
              onNodesChange(c)
              if (c.some((x) => x.type === 'position' || x.type === 'remove')) setDirty(true)
            }}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onNodeClick={(_, n) => setSelection(n.id)}
            onPaneClick={() => setSelection(null)}
            fitView
            minZoom={0.2}
            maxZoom={2}
          >
            <Background />
            <Controls showInteractive={false} />
          </ReactFlow>
        </div>
      </section>

      <aside className="cv-side cv-side--right">
        <div className="panel">
          <div className="panel__title">
            检查器
            {selectedNode && (
              <button
                className="btn btn--danger btn--sm cv-insp__del"
                onClick={() => removeNode(selectedNode.id)}
              >
                删除节点
              </button>
            )}
          </div>
          {!selectedDef && <div className="muted">点一个节点看它的字段</div>}
          {selectedDef && selectedNode && (
            <div className="cv-fields">
              <div className="cv-insp__hd">
                <strong>{selectedDef.label}</strong>
                <span className="mono muted">{selectedNode.id}</span>
              </div>
              {(selectedData?.errors ?? []).map((m) => (
                <div key={m} className="notice notice--error notice__row">
                  {m}
                </div>
              ))}
              {(selectedDef.fields ?? []).map((f) => {
                const value = selectedData?.values?.[f.key]
                const set = (v: unknown) => setField(selectedNode.id, f.key, v)
                return (
                  <label className="field" key={f.key}>
                    <span className="field__label">
                      {f.label}
                      <span className="muted mono"> {f.key}</span>
                    </span>
                    {f.kind === 'textarea' && (
                      <textarea
                        className="textarea"
                        rows={3}
                        value={String(value ?? '')}
                        onChange={(e) => set(e.target.value)}
                      />
                    )}
                    {(f.kind === 'text' || f.kind === 'file') && (
                      <input
                        className="input"
                        value={String(value ?? '')}
                        onChange={(e) => set(e.target.value)}
                      />
                    )}
                    {(f.kind === 'int' || f.kind === 'float') && (
                      <input
                        className="input input--num"
                        type="number"
                        min={f.min}
                        max={f.max}
                        step={f.kind === 'int' ? 1 : 0.05}
                        value={value === undefined || value === null ? '' : String(value)}
                        onChange={(e) => {
                          const raw = e.target.value
                          if (raw === '') return set(undefined)
                          set(f.kind === 'int' ? Number.parseInt(raw, 10) : Number.parseFloat(raw))
                        }}
                      />
                    )}
                    {f.kind === 'bool' && (
                      <input
                        type="checkbox"
                        checked={Boolean(value)}
                        onChange={(e) => set(e.target.checked)}
                      />
                    )}
                    {f.kind === 'enum' && (
                      <select
                        className="select"
                        value={String(value ?? f.options?.[0] ?? '')}
                        onChange={(e) => set(e.target.value)}
                      >
                        {(f.options ?? []).map((o) => (
                          <option key={o} value={o}>
                            {o}
                          </option>
                        ))}
                      </select>
                    )}
                    {f.hint && <span className="field__hint">{f.hint}</span>}
                  </label>
                )
              })}
            </div>
          )}
        </div>

        <div className="panel">
          <div className="panel__title">体检与估算</div>
          {!result && <div className="muted">点「干跑（不提交）」看依赖、耗时区间与问题</div>}
          {result && (
            <div className="cv-report">
              <div className={`notice ${result.run_ready ? 'notice--ok' : 'notice--error'}`}>
                {summarizeReport(result.report)}
                {result.probe_state === 'skipped' && ' · 依赖未探测'}
              </div>
              {est && (
                <div className="cv-est">
                  <div className="cv-est__main">{est.main}</div>
                  <div className="muted2">{est.detail}</div>
                  <div className="muted2">{result.estimate.caveat}</div>
                </div>
              )}
              {(result.deps ?? []).length > 0 && (
                <div className="cv-deps">
                  {(result.deps ?? []).map((d, i) => (
                    <div key={`${d.dep}-${i}`} className="issue">
                      <div className="issue__hd">
                        {d.label}（{d.dep}）
                      </div>
                      <div className="issue__fix">{d.fix}</div>
                    </div>
                  ))}
                </div>
              )}
              {costliestLines(result.estimate)
                .slice(0, 4)
                .map((l) => (
                  <div className="kv" key={l.node_id}>
                    <span className="mono">
                      {l.node_id} · {l.node_type}
                    </span>
                    <span>
                      {humanDuration(l.seconds)}
                      {l.note ? `（${l.note}）` : ''}
                    </span>
                  </div>
                ))}
              {(result.estimate.uncertain ?? []).map((u) => (
                <div key={u} className="muted2">
                  ⚠ {u}
                </div>
              ))}
            </div>
          )}
        </div>
      </aside>
    </div>
  )
}
