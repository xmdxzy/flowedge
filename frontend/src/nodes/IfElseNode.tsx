import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function IfElseNode({ data, selected }: NodeProps) {
  const status = data.status as string || 'pending'

  const statusColor = {
    pending: 'bg-gray-500',
    running: 'bg-primary animate-pulse',
    success: 'bg-success',
    failed: 'bg-error',
  }[status] || 'bg-gray-500'

  const branch = data.branch as string

  return (
    <div
      className={`min-w-[220px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-warning/20 to-warning/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">⑂</span>
        <span className="text-xs font-semibold text-white">条件分支</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">IF/ELSE</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <p className="text-xs text-gray-400 mb-1">条件</p>
        <p className="text-sm text-white">
          {data.condition_type === 'contains' && `包含 "${data.compare_value || ''}"`}
          {data.condition_type === 'not_empty' && '不为空'}
          {data.condition_type === 'equals' && `等于 "${data.compare_value || ''}"`}
        </p>
        {branch && (
          <p className={`text-xs mt-1 ${branch === 'true' ? 'text-success' : 'text-error'}`}>
            → 走 {branch === 'true' ? 'TRUE' : 'FALSE'} 分支
          </p>
        )}
      </div>

      {/* 输入锚点 */}
      <Handle type="target" position={Position.Left} className="!bg-warning" />

      {/* 两个输出锚点 */}
      <Handle
        type="source"
        position={Position.Right}
        id="true"
        style={{ top: '30%' }}
        className="!bg-success"
      />
      <Handle
        type="source"
        position={Position.Right}
        id="false"
        style={{ top: '70%' }}
        className="!bg-error"
      />
    </div>
  )
}

export default memo(IfElseNode)
