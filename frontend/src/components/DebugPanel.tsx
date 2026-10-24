interface DebugPanelProps {
  debugMode: boolean
  runId: number | null
  runStatus: string // '' | 'running' | 'paused' | 'success' | 'failed'
  pausedAt: string
  pauseReason: string
  breakpointCount: number
  busy: boolean
  onStartDebug: () => void
  onStep: () => void
  onContinue: () => void
  onStop: () => void
}

const REASON_LABEL: Record<string, string> = {
  step: '单步',
  breakpoint: '断点',
}

// 调试条：真实驱动后端（POST /api/runs/:id/step|continue），不是本地假状态
export default function DebugPanel({
  debugMode,
  runId,
  runStatus,
  pausedAt,
  pauseReason,
  breakpointCount,
  busy,
  onStartDebug,
  onStep,
  onContinue,
  onStop,
}: DebugPanelProps) {
  const paused = runStatus === 'paused'
  const running = runStatus === 'running'

  if (!debugMode && !paused && !running) {
    return (
      <button
        onClick={onStartDebug}
        className="px-3 py-1.5 rounded-lg bg-warning/15 hover:bg-warning/25 text-warning text-sm border border-warning/40 transition-colors"
        title="开启调试模式：运行到断点会停下，可单步执行"
      >
        🐞 调试{breakpointCount > 0 ? ` (${breakpointCount})` : ''}
      </button>
    )
  }

  return (
    <div
      className={`flex items-center gap-2 px-3 py-1.5 rounded-lg border ${
        paused ? 'bg-warning/10 border-warning/40' : 'bg-surface border-border'
      }`}
    >
      <span className={`text-xs font-medium ${paused ? 'text-warning' : 'text-gray-400'}`}>
        {paused
          ? `${REASON_LABEL[pauseReason] || '暂停'}于 ${pausedAt || '未知节点'}`
          : running
            ? '执行中…'
            : `调试就绪（断点 ${breakpointCount}）`}
      </span>

      <button
        onClick={onStep}
        disabled={!runId || running || busy}
        className="px-2 py-1 rounded bg-canvas hover:bg-surface-hover text-xs disabled:opacity-30"
        title="执行一个节点后停下"
      >
        单步
      </button>
      <button
        onClick={onContinue}
        disabled={!runId || running || busy}
        className="px-2 py-1 rounded bg-canvas hover:bg-surface-hover text-xs disabled:opacity-30"
        title="跑到下一个断点或结束"
      >
        继续
      </button>
      <button
        onClick={onStop}
        disabled={!runId || (!running && !paused)}
        className="px-2 py-1 rounded bg-error/20 hover:bg-error/30 text-xs text-error disabled:opacity-30"
        title="中止这次运行"
      >
        停止
      </button>
    </div>
  )
}
