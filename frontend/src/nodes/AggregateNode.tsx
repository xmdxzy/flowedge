import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

const MODE_LABEL: Record<string, string> = {
  object: '按节点汇聚',
  list: '列表',
  text: '文本拼接',
  first: '取第一个',
  last: '取最后一个',
}

function AggregateNode({ data, selected }: NodeProps) {
  const status = (data.status as string) || 'pending'
  const statusColor =
    {
      pending: 'bg-gray-500',
      running: 'bg-primary animate-pulse',
      success: 'bg-success',
      failed: 'bg-error',
    }[status] || 'bg-gray-500'

  const mode = (data.mode as string) || 'object'
  const field = (data.field as string) || 'text'
  const inputs = (data.inputs as string) || ''

  return (
    <div
      className={`min-w-[210px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status !== 'pending' ? 'node-' + status : ''}
      `}
    >
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-slate-400/20 to-slate-400/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">⋃</span>
        <span className="text-xs font-semibold text-white">变量聚合</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">AGG</span>
      </div>

      <div className="px-3 py-2 space-y-1">
        <div className="flex items-center justify-between">
          <span className="text-[10px] text-gray-500">模式</span>
          <span className="text-[11px] text-gray-300">{MODE_LABEL[mode] || mode}</span>
        </div>
        <div className="flex items-center justify-between">
          <span className="text-[10px] text-gray-500">取值字段</span>
          <span className="text-[11px] text-gray-300 font-mono">{field}</span>
        </div>
        <div className="flex items-center justify-between">
          <span className="text-[10px] text-gray-500">输入</span>
          <span className="text-[11px] text-gray-300 truncate max-w-[120px]" title={inputs}>
            {inputs || '全部上游'}
          </span>
        </div>
        {status === 'success' && data.output != null ? (
          <p className="text-[10px] text-success truncate">
            汇聚 {(data.output as any)?.count ?? 0} 路
          </p>
        ) : null}
      </div>

      <Handle type="target" position={Position.Left} className="!bg-gray-400" />
      <Handle type="source" position={Position.Right} className="!bg-gray-400" />
    </div>
  )
}

export default memo(AggregateNode)
