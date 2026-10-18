import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function RAGNode({ data, selected }: NodeProps) {
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
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-indigo-500/20 to-indigo-500/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">⌕</span>
        <span className="text-xs font-semibold text-white">知识库检索</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">RAG</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <p className="text-xs text-gray-400 mb-1">查询</p>
        <p className="text-sm text-white truncate max-w-[180px]">
          {(data.query as string) || '使用上游输入'}
        </p>
      </div>

      {/* 输入锚点 */}
      <Handle type="target" position={Position.Left} className="!bg-indigo-500" />
      {/* 输出锚点 */}
      <Handle type="source" position={Position.Right} className="!bg-indigo-500" />
    </div>
  )
}

export default memo(RAGNode)
