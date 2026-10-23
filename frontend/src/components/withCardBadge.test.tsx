import { test } from 'node:test'
import assert from 'node:assert/strict'

import type { NodeTypes } from '@xyflow/react'

import withCardBadge from './withCardBadge'
import { mkNode, renderCanvas } from '../test-canvas'
import { setCardIndex } from '../lib/cards'
import { setModules } from '../lib/modules'

// HOC 是所有节点共用的那一层：它挂了，整个画布就白屏。
// 这里在真实 React Flow 容器里渲染，验证三件事：
//   1. 有卡就出徽章；2. 有接线就出"← 上游"；3. 都没有时不多包一层。

setModules([{ type: 'start', title: '开始' }, { type: 'llm', title: 'AI 推理' }])
setCardIndex({
  agents: [{ id: 'a1', name: '导演', role: '', system: '', category: '' }],
  skills: [{ id: 's1', name: '中译英', desc: '', system: '', category: '' }],
  workflows: [],
})

function Base(props: any) {
  return <div data-testid="base">{String((props.data as any)?.label ?? 'node')}</div>
}
const types: NodeTypes = { llm: withCardBadge(Base) }

test('有卡时渲染徽章', () => {
  const { container } = renderCanvas(
    [mkNode('n1', 'llm', { bindings: [{ kind: 'skill', id: 's1', stage: 'input' }] })],
    types,
  )
  assert.ok(container.textContent?.includes('中译英'), '徽章没渲染')
})

test('有接线时渲染上下游', () => {
  const { container } = renderCanvas(
    [mkNode('s1', 'start'), mkNode('n1', 'llm')],
    types,
    [{ id: 'e1', source: 's1', target: 'n1' }],
  )
  assert.ok(container.textContent?.includes('← 开始'), `缺上游提示：${container.textContent}`)
})

test('没卡没接线时只渲染本体，不多包一层', () => {
  const { container } = renderCanvas([mkNode('n9', 'llm')], types)
  assert.equal(container.querySelectorAll('[data-testid="base"]').length, 1)
  assert.ok(!container.querySelector('.fe-node-badges'), '不该出现徽章')
})
