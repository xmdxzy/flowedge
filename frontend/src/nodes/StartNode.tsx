import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function StartNode({ data, selected }: NodeProps) {
  return (
    <div
      className={`min-w-[180px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-control/20 to-control/5 px-3 py-2 border-b border-border">
        <div className="w-2 h-2 rounded-full bg-control" />
        <span className="fe-node-icon">▶</span>
        <span className="text-xs font-semibold text-white">开始</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <p className="text-xs text-gray-400 mb-1">输入提示词</p>
        <p className="text-sm text-white truncate max-w-[160px]">
          {(data.prompt as string) || '点击配置'}
        </p>
      </div>

      {/* 输出锚点 */}
      <Handle
        type="source"
        position={Position.Right}
        className="!bg-control"
      />
    </div>
  )
}

export default memo(StartNode)
