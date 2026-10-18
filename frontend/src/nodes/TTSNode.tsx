import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function TTSNode({ data, selected }: NodeProps) {
  const status = data.status as string || 'pending'

  const statusColor = {
    pending: 'bg-gray-500',
    running: 'bg-primary animate-pulse',
    success: 'bg-success',
    failed: 'bg-error',
  }[status] || 'bg-gray-500'

  return (
    <div
      className={`min-w-[200px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status === 'running' ? 'node-running' : ''}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-success/20 to-success/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">◍</span>
        <span className="text-xs font-semibold text-white">语音合成</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">TTS</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <p className="text-xs text-gray-400 mb-1">文本内容</p>
        <p className="text-sm text-white truncate max-w-[180px]">
          {(data.text as string) || '使用上游文本'}
        </p>
      </div>

      {/* 输入锚点 */}
      <Handle type="target" position={Position.Left} className="!bg-success" />
      {/* 输出锚点 */}
      <Handle type="source" position={Position.Right} className="!bg-success" />
    </div>
  )
}

export default memo(TTSNode)
