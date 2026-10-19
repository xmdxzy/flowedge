import { memo } from 'react'
import { type NodeProps } from '@xyflow/react'

// 分组容器：React Flow 的子节点靠 parentId 定位，这个组件只负责画一个带标题的框。
function GroupNode({ data, selected }: NodeProps) {
  const label = (data.label as string) || '分组'
  return (
    <div
      className={`w-full h-full rounded-[14px] border border-dashed transition-colors ${
        selected ? 'border-primary bg-primary/10' : 'border-primary/40 bg-primary/5'
      }`}
    >
      <span className="fe-group-label">{label}</span>
    </div>
  )
}

export default memo(GroupNode)
