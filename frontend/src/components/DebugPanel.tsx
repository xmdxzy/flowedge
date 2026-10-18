interface DebugPanelProps {
  isDebugging: boolean
  pausedNodeId: string | null
  onStartDebug: () => void
  onPause: () => void
  onResume: () => void
  onStep: () => void
  onStop: () => void
}

export default function DebugPanel({
  isDebugging,
  pausedNodeId,
  onStartDebug,
  onPause,
  onResume,
  onStep,
  onStop,
}: DebugPanelProps) {
  if (!isDebugging) {
    return (
      <button
        onClick={onStartDebug}
        className="px-3 py-1.5 rounded-lg bg-warning/20 hover:bg-warning/30 text-warning text-sm transition-all"
      >
        🐞 调试
      </button>
    )
  }

  return (
    <div className="flex items-center gap-2 px-3 py-1.5 rounded-lg bg-warning/10 border border-warning/30">
      <span className="text-xs text-warning font-medium">
        {pausedNodeId ? `已暂停: ${pausedNodeId}` : '调试中...'}
      </span>
      <button
        onClick={onStep}
        disabled={!pausedNodeId}
        className="px-2 py-1 rounded bg-canvas hover:bg-surface-hover text-xs disabled:opacity-30"
      >
        单步
      </button>
      <button
        onClick={pausedNodeId ? onResume : onPause}
        className="px-2 py-1 rounded bg-canvas hover:bg-surface-hover text-xs"
      >
        {pausedNodeId ? '继续' : '暂停'}
      </button>
      <button
        onClick={onStop}
        className="px-2 py-1 rounded bg-error/20 hover:bg-error/30 text-xs text-error"
      >
        停止
      </button>
    </div>
  )
}
