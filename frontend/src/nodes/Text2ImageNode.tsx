import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function Text2ImageNode({ data, selected }: NodeProps) {
  const status = data.status as string || 'pending'

  const statusColor = {
    pending: 'bg-gray-500',
    running: 'bg-primary animate-pulse',
    success: 'bg-success',
    failed: 'bg-error',
  }[status] || 'bg-gray-500'

  // 产物在 data.output 里（运行结束后由 App 写回），旧代码读的是 data.image，
  // 导致"跑完了节点上不显示缩略"。两种位置都认，兼容历史数据。
  const out = data.output as Record<string, any> | undefined
  const image = (out?.image as string) || (data.image as string)

  // 头部右侧原来硬编码 "SDXL"：引擎默认权重是 flux-schnell，这个标签是错的
  // （与之前"涡轮 LoRA 8 步"同类的 UI 落后于实现）。改成显示真实生效的尺寸与张数。
  const w = Number(data.width ?? 1024)
  const h = Number(data.height ?? 1024)
  const batch = Number(data.batch ?? 1)
  const spec = `${w}×${h}${batch > 1 ? ` · ${batch}张` : ''}`

  return (
    <div
      className={`min-w-[220px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status === 'running' ? 'node-running' : ''}
      `}
    >
      {/* 头部 */}
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-data/20 to-data/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">◈</span>
        <span className="text-xs font-semibold text-white">文生图</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">{spec}</span>
      </div>

      {/* 内容 */}
      <div className="px-3 py-2">
        {image ? (
          <div className="w-full h-20 rounded bg-canvas flex items-center justify-center mb-2">
            <span className="text-xs text-data">🖼️ 已生成</span>
          </div>
        ) : null}
        <p className="text-xs text-gray-400 mb-1">提示词</p>
        <p className="text-sm text-white truncate max-w-[200px]">
          {(data.prompt as string) || '使用上游文本'}
        </p>
      </div>

      {/* 输入锚点 */}
      <Handle type="target" position={Position.Left} className="!bg-data" />
      {/* 输出锚点 */}
      <Handle type="source" position={Position.Right} className="!bg-data" />
    </div>
  )
}

export default memo(Text2ImageNode)
