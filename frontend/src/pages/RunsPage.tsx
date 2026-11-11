import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import { cancelRun, errorText, fetchRun, fetchRuns, rerunRun, resumeRun } from '../api/client'
import type { NodeState, Pipeline, RunDetail, RunSummary } from '../api/types'
import ErrorNotice from '../components/ErrorNotice'
import { toast } from '../components/Toast'
import { useRealtime } from '../hooks/useRealtime'
import { elapsedMs, formatDurationMs, formatTime } from '../lib/format'
import { belongsToRun, createDebouncer } from '../lib/ws'

// 运行看板：列表 + 详情 + 实时刷新。
//
// 实时只做一件事：当前聚焦的 run 收到 WS 事件 → 300ms 去抖后重取一次详情与列表。
// 断线不影响看数据（REST 仍可手动刷新），所以断开时只标注，不阻塞。

const PAGE_SIZE = 50

const STATUS_FILTERS: { value: string; label: string }[] = [
  { value: '', label: '全部' },
  { value: 'running', label: '进行中' },
  { value: 'success', label: '成功' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
]

const STATUS_LABEL: Record<string, string> = {
  pending: '排队中',
  running: '进行中',
  paused: '已暂停',
  success: '成功',
  failed: '失败',
  cancelled: '已取消',
  skipped: '跳过',
  interrupted: '已中断',
}

function statusLabel(status: string): string {
  return STATUS_LABEL[status] ?? status
}

function statusClass(status: string): string {
  switch (status) {
    case 'running':
    case 'success':
    case 'failed':
    case 'cancelled':
    case 'paused':
      return `chip--${status}`
    default:
      return 'chip--quiet'
  }
}

function formatParam(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

interface Props {
  pipelines: Pipeline[]
  focusRunId: number | null
  onFocusRun: (runId: number) => void
}

export default function RunsPage({ pipelines, focusRunId, onFocusRun }: Props) {
  const [status, setStatus] = useState('')
  const [runs, setRuns] = useState<RunSummary[]>([])
  const [total, setTotal] = useState(0)
  const [listLoading, setListLoading] = useState(false)
  const [listError, setListError] = useState('')

  const [detail, setDetail] = useState<RunDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState('')

  const [actionBusy, setActionBusy] = useState(false)
  const [now, setNow] = useState(() => Date.now())

  const nameById = useMemo(() => {
    const map = new Map<string, string>()
    for (const p of pipelines) map.set(p.id, p.name)
    return map
  }, [pipelines])

  const pipelineName = useCallback(
    (id?: string) => (id ? nameById.get(id) ?? id : '—'),
    [nameById],
  )

  const loadList = useCallback(
    async (offset: number, append: boolean) => {
      setListLoading(true)
      try {
        const data = await fetchRuns({ limit: PAGE_SIZE, offset, status })
        setRuns((prev) => (append ? [...prev, ...data.runs] : data.runs))
        setTotal(data.total)
        setListError('')
      } catch (err) {
        setListError(errorText(err))
      } finally {
        setListLoading(false)
      }
    },
    [status],
  )

  const loadDetail = useCallback(async (runId: number, quiet = false) => {
    if (!quiet) setDetailLoading(true)
    try {
      const data = await fetchRun(runId)
      setDetail(data)
      setDetailError('')
    } catch (err) {
      setDetailError(errorText(err))
    } finally {
      if (!quiet) setDetailLoading(false)
    }
  }, [])

  // 筛选变化 → 回到第一页
  useEffect(() => {
    void loadList(0, false)
  }, [loadList])

  // 聚焦哪条就取哪条的详情（出片提交后会直接把 focus 指到新 run）
  useEffect(() => {
    if (focusRunId == null) {
      setDetail(null)
      setDetailError('')
      return
    }
    void loadDetail(focusRunId)
  }, [focusRunId, loadDetail])

  // 每秒走一次：running 的耗时与已跑时长要看着在动
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])

  // 后端没有 run 级 finished 事件（只有 node_*），所以只要还有 running 就低频轮询兜底：
  // 否则最后一条 node_finished 之后界面会一直停在「进行中」。
  const hasRunning = runs.some((run) => run.status === 'running') || detail?.status === 'running'
  useEffect(() => {
    if (!hasRunning) return
    const timer = setInterval(() => {
      void loadList(0, false)
      if (focusRunId != null) void loadDetail(focusRunId, true)
    }, 5000)
    return () => clearInterval(timer)
  }, [hasRunning, focusRunId, loadList, loadDetail])

  // 实时：只有「当前聚焦 run」的事件才触发刷新，且 300ms 去抖
  const refreshRef = useRef<() => void>(() => {})
  refreshRef.current = () => {
    void loadList(0, false)
    if (focusRunId != null) void loadDetail(focusRunId, true)
  }
  const debounceRef = useRef<ReturnType<typeof createDebouncer> | null>(null)
  useEffect(() => {
    debounceRef.current = createDebouncer(300, () => refreshRef.current())
    return () => debounceRef.current?.cancel()
  }, [])

  const { connected } = useRealtime((event) => {
    if (belongsToRun(event, focusRunId)) debounceRef.current?.trigger()
  })

  // 从出片页切过来时，把聚焦行滚进视野
  useEffect(() => {
    if (focusRunId == null) return
    const el = document.getElementById(`run-row-${focusRunId}`)
    el?.scrollIntoView?.({ block: 'nearest' })
  }, [focusRunId, runs.length])

  const handleCancel = async (runId: number) => {
    setActionBusy(true)
    try {
      await cancelRun(runId)
      toast(`已请求中止 #${runId}`, 'success')
      await loadDetail(runId, true)
      await loadList(0, false)
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setActionBusy(false)
    }
  }

  const handleRerun = async (runId: number) => {
    setActionBusy(true)
    try {
      const accepted = await rerunRun(runId)
      toast(`已重跑，新运行 #${accepted.run_id}`, 'success')
      onFocusRun(accepted.run_id)
      await loadList(0, false)
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setActionBusy(false)
    }
  }

  // 断点续跑：引擎被外部杀掉时（interrupted）重跑等于从零再烧一遍，
  // 续跑只补没成功的节点——同一个页面里给用户这条正确的路。
  const handleResume = async (runId: number) => {
    setActionBusy(true)
    try {
      const accepted = await resumeRun(runId)
      const reused = accepted.reused_nodes ? `，复用 ${accepted.reused_nodes} 个已完成节点` : ''
      toast(`已从断点续跑 #${runId}${reused}`, 'success')
      onFocusRun(runId)
      await loadDetail(runId, true)
      await loadList(0, false)
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setActionBusy(false)
    }
  }

  const handleCopy = async (runId: number) => {
    try {
      await navigator.clipboard.writeText(String(runId))
      toast(`已复制 run_id：${runId}`, 'success')
    } catch {
      toast(`复制失败，run_id 是 ${runId}`, 'error')
    }
  }

  return (
    <div className="page page--runs">
      <div className="runs-bar">
        <div className="filters">
          {STATUS_FILTERS.map((f) => (
            <button
              key={f.value}
              className={`chip chip--filter ${status === f.value ? 'is-on' : ''}`}
              onClick={() => setStatus(f.value)}
            >
              {f.label}
            </button>
          ))}
        </div>
        <div className="runs-bar__right">
          {connected ? (
            <span className="chip chip--quiet">
              <i className="dot dot--ok" aria-hidden="true" />
              实时
            </span>
          ) : (
            <span className="chip chip--warn" title="WebSocket 未连接，正在按指数退避重连（最长 30 秒）">
              实时已断开
            </span>
          )}
          <span className="muted2">共 {total} 条</span>
          <button
            className="btn btn--ghost btn--sm"
            onClick={() => {
              void loadList(0, false)
              if (focusRunId != null) void loadDetail(focusRunId, true)
            }}
          >
            刷新
          </button>
        </div>
      </div>

      {listError && (
        <div className="notice notice--error" role="alert">
          <p className="notice__title">运行列表读取失败</p>
          <pre className="notice__raw mono">{listError}</pre>
        </div>
      )}

      <div className="runs-layout">
        <section className="run-list scroll">
          {runRowEmpty(runs, listLoading)}
          {runs.map((run) => {
            const active = run.id === focusRunId
            const duration = elapsedMs(run.started_at, run.ended_at, now)
            return (
              <button
                key={run.id}
                id={`run-row-${run.id}`}
                className={`run-row ${active ? 'is-focus' : ''}`}
                onClick={() => onFocusRun(run.id)}
              >
                <span className={`chip ${statusClass(run.status)}`}>{statusLabel(run.status)}</span>
                <span className="run-row__name">{pipelineName(run.pipeline_id)}</span>
                <span className="run-row__id mono muted2">#{run.id}</span>
                <span className="run-row__time muted2">{formatTime(run.created_at)}</span>
                <span className="run-row__dur mono">
                  {run.status === 'running' ? `${formatDurationMs(duration)}…` : formatDurationMs(duration)}
                </span>
              </button>
            )
          })}
          {runs.length < total && (
            <button
              className="btn btn--ghost btn--sm run-more"
              disabled={listLoading}
              onClick={() => void loadList(runs.length, true)}
            >
              {listLoading ? '加载中…' : `加载更多（${runs.length}/${total}）`}
            </button>
          )}
        </section>

        <section className="run-detail scroll">
          {detailError && (
            <div className="notice notice--error" role="alert">
              <p className="notice__title">运行详情读取失败</p>
              <pre className="notice__raw mono">{detailError}</pre>
            </div>
          )}
          {!detail && !detailError && (
            <p className="empty">
              {detailLoading ? '读取中…' : focusRunId == null ? '从左侧选一条运行看详情' : '读取中…'}
            </p>
          )}
          {detail && (
            <RunDetailView
              detail={detail}
              pipelineName={pipelineName(detail.pipeline)}
              now={now}
              busy={actionBusy}
              onCancel={() => void handleCancel(detail.run_id)}
              onResume={() => void handleResume(detail.run_id)}
              onRerun={() => void handleRerun(detail.run_id)}
              onCopy={() => void handleCopy(detail.run_id)}
              onRefresh={() => void loadDetail(detail.run_id)}
            />
          )}
        </section>
      </div>
    </div>
  )
}

function runRowEmpty(runs: RunSummary[], loading: boolean) {
  if (runs.length > 0 || loading) return null
  return <p className="empty">还没有运行记录。去「出片」页提交一条。</p>
}

interface DetailProps {
  detail: RunDetail
  pipelineName: string
  now: number
  busy: boolean
  onCancel: () => void
  onResume: () => void
  onRerun: () => void
  onCopy: () => void
  onRefresh: () => void
}

function RunDetailView({ detail, pipelineName, now, busy, onCancel, onResume, onRerun, onCopy, onRefresh }: DetailProps) {
  const running = detail.status === 'running'
  // 中断的运行只该给「继续」：成功的节点已经算过钱，从零重跑是浪费
  const canResume = detail.status === 'interrupted'
  const canRerun =
    detail.status === 'success' ||
    detail.status === 'failed' ||
    detail.status === 'cancelled' ||
    detail.status === 'interrupted'
  const duration = elapsedMs(detail.started_at, detail.ended_at, now)
  const nodes: NodeState[] = detail.node_states ?? []
  const params = Object.entries(detail.params ?? {})

  return (
    <div className="detail">
      <div className="detail__hd">
        <span className={`chip ${statusClass(detail.status)}`}>{statusLabel(detail.status)}</span>
        <h2 className="detail__title">{pipelineName}</h2>
        <span className="mono muted2">#{detail.run_id}</span>
        <div className="detail__actions">
          <button className="btn btn--ghost btn--sm" onClick={onCopy}>
            复制 run_id
          </button>
          <button className="btn btn--ghost btn--sm" onClick={onRefresh}>
            刷新
          </button>
          {running && (
            <button className="btn btn--danger btn--sm" disabled={busy} onClick={onCancel}>
              中止
            </button>
          )}
          {canResume && (
            <button className="btn btn--primary btn--sm" disabled={busy} onClick={onResume}>
              从断点继续
            </button>
          )}
          {canRerun && (
            <button className="btn btn--ghost btn--sm" disabled={busy} onClick={onRerun}>
              重跑
            </button>
          )}
        </div>
      </div>

      <div className="facts">
        <div>
          <span className="muted2">开始 </span>
          <span className="mono">{formatTime(detail.started_at)}</span>
        </div>
        <div>
          <span className="muted2">结束 </span>
          <span className="mono">{formatTime(detail.ended_at)}</span>
        </div>
        <div>
          <span className="muted2">耗时 </span>
          <span className="mono">
            {formatDurationMs(duration)}
            {running ? '（进行中）' : ''}
          </span>
        </div>
        <div>
          <span className="muted2">触发 </span>
          <span className="mono">{detail.invoked_by || '—'}</span>
        </div>
      </div>

      {detail.error && (
        <ErrorNotice className="detail__error" message={detail.error} compact={false} />
      )}

      <section className="detail__section">
        <h3 className="detail__subtitle">生效参数</h3>
        {params.length === 0 ? (
          <p className="muted">没有参数</p>
        ) : (
          <table className="kv">
            <tbody>
              {params.map(([key, value]) => (
                <tr key={key}>
                  <th className="mono">{key}</th>
                  <td className="mono">{formatParam(value)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="detail__section">
        <h3 className="detail__subtitle">
          节点 <span className="muted2">{nodes.length}</span>
        </h3>
        {nodes.length === 0 ? (
          <p className="muted">这次运行没有节点状态记录（可能还在排队）。</p>
        ) : (
          <ol className="nodes">
            {nodes.map((node) => {
              const nodeDuration = elapsedMs(node.started_at, node.ended_at, now)
              const failed = node.status === 'failed' || Boolean(node.error)
              return (
                <li key={node.id} className={`node-row ${failed ? 'is-failed' : ''}`}>
                  <span className={`chip ${statusClass(node.status)}`}>{statusLabel(node.status)}</span>
                  <span className="node-row__id mono">{node.id}</span>
                  <span className="node-row__type muted2">{node.type}</span>
                  <span className="node-row__dur mono">{formatDurationMs(nodeDuration)}</span>
                  {node.error_code && <span className="chip chip--quiet mono">{node.error_code}</span>}
                  {node.error && <pre className="node-row__err mono">{node.error}</pre>}
                </li>
              )
            })}
          </ol>
        )}
      </section>
    </div>
  )
}
