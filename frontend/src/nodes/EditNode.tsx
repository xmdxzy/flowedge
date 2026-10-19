import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

// 四个「图像/视频编辑」节点共用一个组件：结构一致，只是标题与字段不同。
// 单独拆四个文件只会产生四份几乎相同的 JSX。
const SPEC: Record<string, { title: string; icon: string; tag: string; tone: string; hint: string }> = {
  image2image: { title: '图生图', icon: '🖌', tag: 'I2I', tone: 'from-ai/20 to-ai/5', hint: '强度' },
  inpaint: { title: '局部重绘', icon: '🩹', tag: 'INP', tone: 'from-warning/20 to-warning/5', hint: '去噪' },
  upscale: { title: '超分放大', icon: '⤢', tag: 'UP', tone: 'from-success/20 to-success/5', hint: '模型' },
  text2video: { title: '文生视频', icon: '🎬', tag: 'T2V', tone: 'from-purple/20 to-purple/5', hint: '帧数' },
}

function EditNode({ type, data, selected }: NodeProps) {
  const spec = SPEC[type || ''] || SPEC.image2image
  const status = (data.status as string) || 'pending'
  const statusColor =
    {
      pending: 'bg-gray-500',
      running: 'bg-primary animate-pulse',
      success: 'bg-success',
      failed: 'bg-error',
    }[status] || 'bg-gray-500'

  const out = data.output as any
  const line2 = (() => {
    if (type === 'image2image') return `强度 ${data.strength ?? 0.6}`
    if (type === 'inpaint') return (data.mask_path as string) ? '已配蒙版' : '缺蒙版'
    if (type === 'upscale') return (data.model as string) || 'RealESRGAN_x4plus'
    return `${data.width ?? 832}×${data.height ?? 480} · ${data.frames ?? 33} 帧`
  })()

  return (
    <div
      className={`min-w-[210px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status === 'running' ? 'node-running' : ''}
      `}
    >
      <div className={`flex items-center gap-2 rounded-t-lg bg-gradient-to-r ${spec.tone} px-3 py-2 border-b border-border`}>
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">{spec.icon}</span>
        <span className="text-xs font-semibold text-white">{spec.title}</span>
        <span className="ml-auto text-[10px] text-gray-500 font-mono">{spec.tag}</span>
      </div>

      <div className="px-3 py-2">
        <p className="text-[11px] text-gray-300 line-clamp-1">
          {(data.prompt as string) || '（用上游提示词）'}
        </p>
        <div className="flex items-center justify-between mt-1">
          <span className="text-[10px] text-gray-500">{spec.hint}</span>
          <span className="text-[10px] text-gray-400 font-mono truncate max-w-[120px]">{line2}</span>
        </div>
        {out?.image ? (
          <p className="mt-1 text-[10px] text-success truncate">{String(out.image).split(/[\\/]/).pop()}</p>
        ) : null}
        {out?.video ? (
          <p className="mt-1 text-[10px] text-success truncate">{String(out.video).split(/[\\/]/).pop()}</p>
        ) : null}
      </div>

      <Handle type="target" position={Position.Left} className="!bg-ai" />
      <Handle type="source" position={Position.Right} className="!bg-ai" />
    </div>
  )
}

export default memo(EditNode)
