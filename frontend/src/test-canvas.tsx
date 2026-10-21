// 画布测试 harness：把节点组件放进真实的 React Flow 容器里渲染。
//
// 为什么不直接 render(<StartNode {...props} />)：节点里有 <Handle>，它需要 React Flow 的
// store 与节点上下文；脱离 <ReactFlow> 会报 "you have not used the ReactFlow component"。
// 所以这里包一层 provider + 真画布，断言落在渲染出来的文字上。

import { ReactFlow, ReactFlowProvider, type Edge, type Node, type NodeTypes } from '@xyflow/react'
import { render } from '@testing-library/react'

export function renderCanvas(nodes: Node[], nodeTypes: NodeTypes, edges: Edge[] = []) {
  return render(
    <ReactFlowProvider>
      <div style={{ width: 900, height: 600 }}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView={false}
          proOptions={{ hideAttribution: true }}
        />
      </div>
    </ReactFlowProvider>,
  )
}

/** 造一个最小节点（只填渲染会用到的字段） */
export function mkNode(id: string, type: string, data: Record<string, unknown> = {}): Node {
  return { id, type, position: { x: 0, y: 0 }, data }
}
