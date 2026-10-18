import { useCallback, useEffect, useRef, useState } from 'react'

interface RunRecord {
  id: number
  workflow_id: number
  status: string
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

interface NodeState {
  id: string
  type: string
  status: string
  error?: string
  error_code?: string
  started_at?: string
  ended_at?: string
  output?: any
}

interface RunEventRecord {
  seq: number
  type: string
  node_id?: string
  message?: string
  data?: any
  created_at: string
}

interface Usage {
  prompt_tokens: number
  completion_tokens: number
}

const STATUS_STYLE: Record<string, { dot: string; bar: string; label: string }> = {
  success: { dot: 'bg-success', bar: 'bg-success/70', label: '成功' },
  failed: { dot: 'bg-error', bar: 'bg-error/70', label: '失败' },
  running: { dot: 'bg-primary animate-pulse', bar: 'bg-primary/70 animate-pulse', label: '运行中' },
  skipped: { dot: 'bg-gray-600', bar: 'bg-gray-600/60', label: '跳过' },
  pending: { dot: 'bg-gray-500', bar: 'bg-gray-500/50', label: '等待' },
}

const EVENT_LABEL: Record<string, { icon: string; cls: string }> = {
  node_started: { icon: '▶', cls: 'text-primary' },
  node_finished: { icon: '✓', cls: 'text-success' },
  node_failed: { icon: '✕', cls: 'text-error' },
  node_retry: { icon: '⟳', cls: 'text-warning' },
  tool_call: { icon: '⌗', cls: 'text-purple' },
  loop_iteration: { icon: '↻', cls: 'text-data' },
}

function durationMs(n: NodeState): number {
  if (!n.started_at || !n.ended_at) return 0
  return Math.max(0, new Date(n.ended_at).getTime() - new Date(n.started_at).getTime())
}

function fmtDur(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60000)}m${Math.round((ms % 60000) / 1000)}s`
}

export default function ExecutionPanel({ workflowId }: { workflowId?: number }) {
  const [runs, setRuns] = useState<RunRecord[]>([])
  const [selectedRun, setSelectedRun] = useState<RunRecord | null>(null)
  const [nodeStates, setNodeStates] = useState<NodeState[]>([])
  const [events, setEvents] = useState<RunEventRecord[]>([])
  const [usage, setUsage] = useState<Usage | null>(null)
  const [isOpen, setIsOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  // 用 ref 持有 selectedRun，供轮询闭包读取最新值，避免每次选中都重建定时器
  const selectedRef = useRef<RunRecord | null>(null)
  selectedRef.current = selectedRun

  const loadRuns = useCallback(async () => {
    if (!workflowId) return
    try {
      const resp = await fetch(`/api/workflows/${workflowId}/runs?limit=50`)
      setRuns(await resp.json())
    } catch (err) {
      console.error('failed to load runs', err)
    }
  }, [workflowId])

  useEffect(() => {
    loadRuns()
  }, [loadRuns])

  // 打开面板或有运行中记录时轮询；进行中的运行同时刷新事件流
  useEffect(() => {
    if (!isOpen) return
    const timer = setInterval(() => {
      loadRuns()
      const cur = selectedRef.current
      if (cur && cur.status === 'running') openRun(cur)
    }, 2500)
    return () => clearInterval(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isOpen, loadRuns])

  async function openRun(run: RunRecord) {
    setSelectedRun(run)
    try {
      const resp = await fetch(`/api/runs/${run.id}`)
      const data = await resp.json()
      setNodeStates(Array.isArray(data.node_states) ? data.node_states : [])
    } catch {
      setNodeStates([])
    }
    // 事件流：node_token 不入库，所以这里只回放结构化事件
    try {
      const ev = await fetch(`/api/runs/${run.id}/events`)
      if (ev.ok) {
        const d = await ev.json()
        setEvents(Array.isArray(d.events) ? d.events : [])
        setUsage(d.usage || null)
      } else {
        setEvents([])
        setUsage(null)
      }
    } catch {
      setEvents([])
      setUsage(null)
    }
  }

  const cancelRun = async (run: RunRecord) => {
    setBusy(true)
    try {
      const resp = await fetch(`/api/runs/${run.id}/cancel`, { method: 'POST' })
      if (!resp.ok) {
        const data = await resp.json().catch(() => ({}))
        alert(data.error || '取消失败')
      }
      loadRuns()
    } finally {
      setBusy(false)
    }
  }

  const resumeFromRun = async (run: RunRecord, nodeId: string) => {
    try {
      const resp = await fetch(`/api/runs/${run.id}/rerun-from`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ node_id: nodeId }),
      })
      if (!resp.ok) {
        const d = await resp.json().catch(() => ({}))
        alert(d.error || '重跑失败')
        return
      }
      loadRuns()
      const timer = setTimeout(() => openRun(run), 600)
      return () => clearTimeout(timer)
    } catch (err) {
      alert('重跑失败：' + err)
    }
  }

  const totalMs = nodeStates.reduce((acc, n) => {
    const s = n.started_at ? new Date(n.started_at).getTime() : 0
    const e = n.ended_at ? new Date(n.ended_at).getTime() : 0
    return Math.max(acc, e ? e - (nodeStates[0]?.started_at ? new Date(nodeStates[0].started_at!).getTime() : s) : 0)
  }, 0)

  return (
    <div className="border-t border-border bg-surface">
      <div
        className="flex items-center justify-between px-4 py-2 cursor-pointer hover:bg-surface-hover"
        onClick={() => setIsOpen(!isOpen)}
      >
        <div className="flex items-center gap-2">
          <span className="text-xs font-semibold text-gray-300">执行历史</span>
          <span className="text-[10px] text-gray-500">{runs.length} 条记录</span>
        </div>
        <span className="text-xs text-gray-500">{isOpen ? '▼' : '▲'}</span>
      </div>

      {isOpen && (
        <div className="max-h-72 overflow-hidden flex">
          {/* 运行列表 */}
          <div className="w-[38%] border-r border-border/60 overflow-y-auto">
            {runs.length === 0 ? (
              <p className="px-4 py-3 text-xs text-gray-500">暂无执行记录</p>
            ) : (
              runs.map((run) => {
                const style = STATUS_STYLE[run.status] || STATUS_STYLE.pending
                return (
                  <div
                    key={run.id}
                    className={`px-4 py-2 border-b border-border/50 cursor-pointer hover:bg-surface-hover ${
                      selectedRun?.id === run.id ? 'bg-surface-hover' : ''
                    }`}
                    onClick={() => openRun(run)}
                  >
                    <div className="flex items-center gap-2">
                      <div className={`w-2 h-2 rounded-full ${style.dot}`} />
                      <span className="text-xs text-white">Run #{run.id}</span>
                      <span className="text-[10px] text-gray-500">{style.label}</span>
                      {run.status === 'running' && (
                        <button
                          onClick={(e) => {
                            e.stopPropagation()
                            cancelRun(run)
                          }}
                          disabled={busy}
                          className="ml-auto text-[10px] px-1.5 py-0.5 rounded border border-error/40 text-red-300 hover:bg-error/15"
                        >
                          停止
                        </button>
                      )}
                    </div>
                    <p className="text-[10px] text-gray-600 mt-0.5">
                      {new Date(run.created_at).toLocaleTimeString()}
                      {run.ended_at && run.started_at
                        ? ` · ${fmtDur(new Date(run.ended_at).getTime() - new Date(run.started_at).getTime())}`
                        : ''}
                    </p>
                    {run.error && (
                      <p className="text-[10px] text-error mt-1 truncate" title={run.error}>
                        {run.error}
                      </p>
                    )}
                  </div>
                )
              })
            )}
          </div>

          {/* 右侧：节点耗时 + 事件流 */}
          <div className="flex-1 flex flex-col overflow-hidden">
            {/* 节点耗时瀑布图 */}
            <div className="flex-1 overflow-y-auto px-3 py-2 min-h-0">
              {!selectedRun ? (
                <p className="text-[11px] text-gray-500 mt-1">选择一条记录查看节点耗时</p>
              ) : nodeStates.length === 0 ? (
                <p className="text-[11px] text-gray-500 mt-1">该记录暂无节点明细</p>
              ) : (
                <div className="flex flex-col gap-1.5">
                  <div className="flex items-center justify-between mb-1">
                    <span className="text-[10px] text-gray-400">Run #{selectedRun.id} · 节点耗时</span>
                    <span className="text-[10px] text-gray-600">总 {fmtDur(totalMs)}</span>
                  </div>
                  {nodeStates.map((n) => {
                    const ms = durationMs(n)
                    const style = STATUS_STYLE[n.status] || STATUS_STYLE.pending
                    const pct = totalMs > 0 ? Math.max(3, (ms / totalMs) * 100) : 3
                    return (
                      <div key={n.id} title={n.error || n.error_code || ''}>
                        <div className="flex items-center gap-1.5">
                          <span className="text-[9px] text-gray-400 truncate max-w-[86px]">{n.id}</span>
                          <span className="text-[9px] text-gray-600">{n.type}</span>
                          <span className="ml-auto text-[9px] font-mono text-gray-500">{fmtDur(ms)}</span>
                          <button
                            className="text-[9px] px-1 rounded border border-border text-gray-500 hover:text-primary hover:border-primary/60"
                            title="从这个节点重跑（保留上游结果）"
                            onClick={() => resumeFromRun(selectedRun, n.id)}
                          >
                            重跑
                          </button>
                        </div>
                        <div className="h-1.5 rounded-full bg-canvas mt-0.5 overflow-hidden">
                          <div
                            className={`h-full rounded-full ${style.bar} transition-all duration-300`}
                            style={{ width: `${pct}%` }}
                          />
                        </div>
                      </div>
                    )
                  })}
                </div>
              )}
            </div>

            {/* 事件流（含 token 汇总） */}
            <div className="h-1/2 border-t border-border/60 overflow-y-auto px-3 py-2">
              <div className="flex items-center justify-between mb-1 sticky top-0 bg-surface py-0.5">
                <span className="text-[10px] text-gray-400">事件流</span>
                {usage && (usage.prompt_tokens > 0 || usage.completion_tokens > 0) && (
                  <span className="text-[10px] text-gray-500 font-mono" title="本次运行的 token 消耗">
                    tokens +{usage.prompt_tokens} / ↳{usage.completion_tokens}
                  </span>
                )}
              </div>
              {!selectedRun ? (
                <p className="text-[11px] text-gray-500">选择一条记录查看事件</p>
              ) : events.length === 0 ? (
                <p className="text-[11px] text-gray-500">无事件记录</p>
              ) : (
                events.map((e) => {
                  const meta = EVENT_LABEL[e.type] || { icon: '·', cls: 'text-gray-500' }
                  const tool = e.data?.tool
                  return (
                    <div key={e.seq} className="flex items-center gap-1.5 text-[10px] leading-5">
                      <span className="text-gray-700 font-mono w-5 text-right shrink-0">{e.seq}</span>
                      <span className={meta.cls}>{meta.icon}</span>
                      <span className="text-gray-400 shrink-0">{e.type.replace('node_', '')}</span>
                      <span className="text-gray-500 truncate">
                        {tool ? ` ${tool}` : e.node_id || ''}
                        {e.message ? ` · ${e.message}` : ''}
                      </span>
                      <span className="ml-auto text-gray-700 shrink-0 pl-2">
                        {new Date(e.created_at).toLocaleTimeString()}
                      </span>
                    </div>
                  )
                })
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
