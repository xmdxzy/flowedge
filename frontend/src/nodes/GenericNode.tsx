import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

// 通用节点：给「运行期插件」这类**编译期不知道类型**的节点兜底。
//
// 为什么需要它
//
//	画布上的渲染组件是一张静态表（baseNodeTypes）。插件节点（data/plugins/<name>）
//	的类型名是用户自己起的，静态表里不可能有——于是插件装是装上了、引擎也能跑，
//	但拖到画布上 ReactFlow 会报 "Node type not found"，整张图看起来是坏的。
//	「插件生态」这件事在最后一步断掉，等于没做。
//
// 做法：未知类型一律落到这里渲染。标题取 data._label（拖拽时写入），
// 否则用类型名；参数只显示节点数据里的一两个标量，够认出这是哪个节点。
function GenericNode({ data, type, selected }: NodeProps) {
  const status = (data.status as string) || 'pending'
  const statusColor =
    {
      pending: 'bg-gray-500',
      running: 'bg-primary animate-pulse',
      success: 'bg-success',
      failed: 'bg-error',
    }[status] || 'bg-gray-500'

  const label = (data._label as string) || type
  // 只挑短的标量做摘要：插件的输出/入参可能很大，塞进卡片会撑爆
  const preview = Object.entries(data)
    .filter(([k, v]) => !k.startsWith('_') && (typeof v === 'string' || typeof v === 'number'))
    .filter(([k]) => !['status', 'error', 'output', 'input', 'id', 'type'].includes(k))
    .slice(0, 2)
    .map(([k, v]) => `${k}=${String(v).slice(0, 18)}`)
    .join(' · ')

  return (
    <div
      className={`min-w-[190px] rounded-lg border bg-surface shadow-lg transition-all duration-200 hover:shadow-xl hover:-translate-y-0.5
        ${selected ? 'border-primary shadow-primary-glow' : 'border-border'}
        ${status !== 'pending' ? 'node-' + status : ''}
      `}
    >
      <div className="flex items-center gap-2 rounded-t-lg bg-gradient-to-r from-amber-500/20 to-amber-500/5 px-3 py-2 border-b border-border">
        <div className={`w-2 h-2 rounded-full ${statusColor}`} />
        <span className="fe-node-icon">⚒</span>
        <span className="text-xs font-semibold text-white truncate max-w-[140px]">{label}</span>
        <span className="ml-auto text-[9px] text-gray-500 font-mono">插件</span>
      </div>
      <div className="px-3 py-2">
        <p className="text-[10px] text-gray-500 font-mono truncate" title={type}>
          {type}
        </p>
        <p className="text-[11px] text-gray-400 truncate max-w-[170px]">
          {preview || '参数在右侧属性面板里填'}
        </p>
      </div>
      <Handle type="target" position={Position.Left} className="!bg-amber-500" />
      <Handle type="source" position={Position.Right} className="!bg-amber-500" />
    </div>
  )
}

export default memo(GenericNode)
