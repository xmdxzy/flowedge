import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function EndNode({ data, selected }: NodeProps) {
  const output = data.output as string

  return (
    <div
      className={`min-w-[180px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-output/20 to-output/5 px-3 py-2 border-b border-border">
        <div className="w-2 h-2 rounded-full bg-output" />
        <span className="text-xs font-semibold text-white">结束</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        <p className="text-xs text-gray-400 mb-1">输出结果</p>
        {output ? (
          <p className="text-xs text-success line-clamp-3 max-w-[160px]">
            {output}
          </p>
        ) : (
          <p className="text-sm text-gray-500">等待执行</p>
        )}
      </div>

      {/* 输入锚点 */}
      <Handle
        type="target"
        position={Position.Left}
        className="!bg-output"
      />
    </div>
  )
}

export default memo(EndNode)
