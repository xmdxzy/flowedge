import { useViewport } from '@xyflow/react'

interface Props {
  nodesCount: number
  edgesCount: number
  isRunning: boolean
  runStatus: string
  gpu?: { usedMB?: number; totalMB?: number; utilization?: number } | null
}

// 底部状态栏：只订阅视口与计数，zoom 变化不会重渲染主编辑器
export default function StatusBar({ nodesCount, edgesCount, isRunning, runStatus, gpu }: Props) {
  const { zoom } = useViewport()

  const stateLabel = isRunning ? '运行中' : runStatus || '就绪'
  const stateClass =
    runStatus === 'failed' ? 'sb-err' : runStatus === 'success' ? 'sb-ok' : isRunning ? 'sb-warn' : ''

  return (
    <div className="fe-statusbar">
      <span className="sb-item">
        节点 <b>{nodesCount}</b>
      </span>
      <span className="sb-item">
        连线 <b>{edgesCount}</b>
      </span>
      <span className="sb-sep" />
      <span className={`sb-item ${stateClass}`}>{stateLabel}</span>

      {gpu && (
        <>
          <span className="sb-sep" />
          <span className="sb-item" title="显卡显存占用（nvidia-smi 采样）">
            显存 <b>{gpu.usedMB ?? 0}</b>
            <span className="fe-dim3">/{gpu.totalMB ?? 0} MB</span>
          </span>
          <span className="sb-item">
            利用率 <b>{gpu.utilization ?? 0}%</b>
          </span>
        </>
      )}

      <div className="ml-auto flex items-center gap-3">
        <span className="fe-dim3">拖入建节点 · 空格平移 · Ctrl+K 命令面板</span>
        <span className="sb-sep" />
        <span className="sb-item">
          缩放 <b>{Math.round(zoom * 100)}%</b>
        </span>
      </div>
    </div>
  )
}
