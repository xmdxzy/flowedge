import type { ComponentType } from 'react'
import { useStore } from '@xyflow/react'

import NodeBadges from './NodeBadges'
import { readPipeline, writePipeline } from '../lib/cards'
import { typeLabel } from '../lib/modules'

// 给所有节点套上「能力徽章 + 接线摘要」。
//
// 为什么用 HOC 而不是改 20 个节点组件：徽章是**统一表达**。
// 改组件会得到 20 份略有差异的实现，新加节点必然漏一次。
//
// 为什么接线用"字符串选择器"：React Flow 的 store 每次拖动都在变，
// 如果 selector 返回新数组，20 个节点会跟着整片重渲染（拖动掉帧）。
// 拼成一个字符串后，只有"接线真的变了"才会触发重渲染。
export default function withCardBadge(Base: ComponentType<any>) {
  const Wrapped = (props: any) => {
    const pipe = readPipeline(props.data as Record<string, unknown>)
    const all = writePipeline(pipe)

    const links = useStore((s: any) => {
      const nameOf = (id: string) => typeLabel(s.nodes.find((n: any) => n.id === id)?.type)
      const ins = s.edges.filter((e: any) => e.target === props.id).map((e: any) => nameOf(e.source))
      const outs = s.edges.filter((e: any) => e.source === props.id).map((e: any) => nameOf(e.target))
      return `${ins.join('、')}|${outs.join('、')}`
    })
    const [inText, outText] = String(links).split('|')

    if (all.length === 0 && !inText && !outText) return <Base {...props} />
    return (
      <div className="relative">
        {all.length > 0 && <NodeBadges bindings={all} />}
        <Base {...props} />
        {(inText || outText) && (
          <div className="fe-node-links">
            {inText && <span className="fe-node-links__in">← {inText}</span>}
            {outText && <span className="fe-node-links__out">→ {outText}</span>}
          </div>
        )}
      </div>
    )
  }
  Wrapped.displayName = `withCardBadge(${Base.displayName || Base.name || 'Node'})`
  return Wrapped
}
