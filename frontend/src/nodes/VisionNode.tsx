import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

// 视觉理解节点：把上游图片交给本地 VL 模型，输出文字描述
function VisionNode({ data, selected }: NodeProps) {
  const status = (data.status as string) || 'pending'
  const statusColor =
    {
      pending: 'bg-gray-500',
      running: 'bg-primary animate-pulse',
      success: 'bg-success',
      failed: 'bg-error',
    }[status] || 'bg-gray-500'

  const prompt = (data.prompt as string) || ''
  const out = data.output as any
  const preview = out && typeof out === 'object' ? String(out.text || '') : ''

  return (
    <div
      className={`min-w-[220px] max-w-[260px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status === 'running' ? 'node-running' : ''}
      `}
    >
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-teal-400/20 to-teal-400/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">👁</span>
        <span className="text-xs font-semibold text-white">视觉理解</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">VLM</span>
      </div>

      <div className="px-3 py-2">
        <p className="text-[10px] text-gray-500 mb-0.5">提问</p>
        <p className="text-[11px] text-gray-300 line-clamp-2">
          {prompt || '描述这张图片的主体、构图与风格'}
        </p>
        {preview ? (
          <p className="mt-1.5 text-[10px] text-success line-clamp-2">{preview}</p>
        ) : null}
      </div>

      <Handle type="target" position={Position.Left} className="!bg-teal-400" />
      <Handle type="source" position={Position.Right} className="!bg-teal-400" />
    </div>
  )
}

export default memo(VisionNode)
