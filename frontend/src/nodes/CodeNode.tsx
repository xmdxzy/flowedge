import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function CodeNode({ data, selected }: NodeProps) {
  const status = (data.status as string) || 'pending'

  const statusColor = {
    pending: 'bg-gray-500',
    running: 'bg-primary animate-pulse',
    success: 'bg-success',
    failed: 'bg-error',
  }[status] || 'bg-gray-500'

  const code = (data.code as string) || ''
  const lines = code.trim().split('\n')
  const preview = lines.slice(0, 3).join('\n')
  const hidden = Math.max(0, lines.length - 3)

  return (
    <div
      className={`min-w-[210px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status !== 'pending' ? 'node-' + status : ''}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-data/20 to-data/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">{ }</span>
        <span className="text-xs font-semibold text-white">代码执行</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">Go</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        {code ? (
          <div className="rounded-md bg-black/40 border border-accent/20 px-2 py-1.5 max-h-24 overflow-hidden">
            <pre className="text-[10px] text-cyan-300/90 font-mono whitespace-pre-wrap leading-snug">
              {preview}
            </pre>
            {hidden > 0 && (
              <p className="text-[9px] text-gray-600 mt-1">… 另有 {hidden} 行</p>
            )}
          </div>
        ) : (
          <p className="text-[11px] text-gray-500">未填写代码（result = …）</p>
        )}

        {(data.streamText as string) && (
          <div className="mt-2 max-h-20 overflow-y-auto rounded-md bg-black/40 border border-accent/20 px-2 py-1.5">
            <p className="text-[11px] text-cyan-300 font-mono whitespace-pre-wrap leading-snug">
              {data.streamText as string}
            </p>
          </div>
        )}
      </div>

      {/* 输入锚点 */}
      <Handle type="target" position={Position.Left} className="!bg-data" />
      {/* 输出锚点 */}
      <Handle type="source" position={Position.Right} className="!bg-data" />
    </div>
  )
}

export default memo(CodeNode)
