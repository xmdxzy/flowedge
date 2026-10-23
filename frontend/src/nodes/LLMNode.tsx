import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function LLMNode({ data, selected }: NodeProps) {
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
        ${status !== 'pending' ? 'node-' + status : ''}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-ai/20 to-ai/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">✦</span>
        <span className="text-xs font-semibold text-white">AI 推理</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">LLM</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <div className="flex items-center gap-1.5 mb-1">
          <p className="text-xs text-gray-400 truncate max-w-[130px]">
            模型: {(data.model as string) || 'qwen2.5-coder:32b'}
          </p>
          {((data.tools as string[]) || []).length > 0 && (
            <span className="text-[9px] px-1 py-0.5 rounded bg-ai/20 text-ai shrink-0">
              {(data.tools as string[]).length} 工具
            </span>
          )}
        </div>
        <p className="text-sm text-white truncate max-w-[180px]">
          {(data.prompt as string) || '使用上游输入'}
        </p>
        {(data.streamText as string) && (
          <div className="mt-2 max-h-24 overflow-y-auto rounded-md bg-black/40 border border-accent/20 px-2 py-1.5">
            <p className="text-[11px] text-cyan-300 font-mono whitespace-pre-wrap leading-snug">
              {data.streamText as string}
              <span className="animate-pulse text-accent">▊</span>
            </p>
          </div>
        )}
      </div>

      {/* 输入锚点 */}
      <Handle
        type="target"
        position={Position.Left}
        className="!bg-ai"
      />
      {/* 输出锚点 */}
      <Handle
        type="source"
        position={Position.Right}
        className="!bg-ai"
      />
    </div>
  )
}

export default memo(LLMNode)
