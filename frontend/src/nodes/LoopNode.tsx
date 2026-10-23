import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

const MODE_LABEL: Record<string, string> = {
  count: '固定次数',
  foreach: '遍历列表',
  until: '条件循环',
}

function LoopNode({ data, selected }: NodeProps) {
  const status = (data.status as string) || 'pending'
  const mode = (data.mode as string) || 'count'
  const iteration = Number(data.loop_iteration ?? 0)
  const maxIter = Number(data.max_iterations ?? 20)

  const statusColor =
    {
      pending: 'bg-gray-500',
      running: 'bg-warning animate-pulse',
      success: 'bg-success',
      failed: 'bg-error',
      skipped: 'bg-gray-600',
    }[status] || 'bg-gray-500'

  const subtitle =
    mode === 'count'
      ? `迭代 ${data.iterations ?? 3} 次`
      : mode === 'foreach'
        ? '逐项处理输入'
        : '条件满足即停'

  return (
    <div
      className={`min-w-[210px] rounded-lg border bg-surface shadow-node transition-all duration-200 hover:shadow-node-hover hover:-translate-y-0.5
        ${selected ? 'border-warning shadow-[0_0_0_1px_rgba(251,191,36,0.45),0_0_24px_rgba(251,191,36,0.25)]' : 'border-border'}
        ${status !== 'pending' ? 'node-' + status : ''}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-warning/20 to-warning/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">⟳</span>
        <span className="text-xs font-semibold text-white">循环</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">LOOP</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <div className="flex items-center gap-1.5 mb-1">
          <span className="text-[9px] font-medium px-1.5 py-0.5 rounded bg-warning/15 text-warning">
            {MODE_LABEL[mode] || mode}
          </span>
          {mode === 'until' && (
            <span className="text-[9px] text-gray-500 font-mono truncate max-w-[92px]">
              {String(data.condition_type || 'not_empty')}
            </span>
          )}
        </div>

        <p className="text-xs text-gray-400 truncate max-w-[180px]">
          {(data.workflow_name as string) || '选择要循环的工作流'}
        </p>

        <div className="flex items-center justify-between mt-1.5">
          <span className="text-[10px] text-gray-500">{subtitle}</span>
          {iteration > 0 && (
            <span className="text-[10px] font-mono text-warning">
              {iteration}/{maxIter}
            </span>
          )}
        </div>

        {/* 迭代进度条 */}
        {status === 'running' && iteration > 0 && (
          <div className="mt-1.5 h-1 rounded-full bg-surface-hover overflow-hidden">
            <div
              className="h-full rounded-full bg-gradient-to-r from-warning to-accent transition-all duration-300"
              style={{ width: `${Math.min(100, (iteration / Math.max(1, maxIter)) * 100)}%` }}
            />
          </div>
        )}
      </div>

      <Handle type="target" position={Position.Left} className="!bg-warning" />
      <Handle type="source" position={Position.Right} className="!bg-warning" />
    </div>
  )
}

export default memo(LoopNode)
