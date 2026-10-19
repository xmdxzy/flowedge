import { memo, useState } from 'react'
import { type NodeProps, useReactFlow } from '@xyflow/react'

// 画布批注：纯注释，不进后端、不参与执行。
// textarea 加 nodrag/nowheel，否则在画布里打字会拖动节点。
function NoteNode({ id, data, selected }: NodeProps) {
  const { setNodes } = useReactFlow()
  const [text, setText] = useState((data.text as string) || '')

  const update = (value: string) => {
    setText(value)
    setNodes((nds) => nds.map((n) => (n.id === id ? { ...n, data: { ...n.data, text: value } } : n)))
  }

  return (
    <div className={`fe-note ${selected ? 'ring-2 ring-warning/60' : ''}`}>
      <div className="flex items-center gap-1.5 mb-1">
        <span className="text-[10px] text-warning font-semibold tracking-wide">批注</span>
      </div>
      <textarea
        className="nodrag nowheel"
        rows={3}
        placeholder="写点说明…（不会进入工作流定义）"
        value={text}
        onChange={(e) => update(e.target.value)}
        onMouseDown={(e) => e.stopPropagation()}
      />
    </div>
  )
}

export default memo(NoteNode)
