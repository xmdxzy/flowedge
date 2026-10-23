import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render } from '@testing-library/react'

import NodePipeline from './NodePipeline'
import { fire } from '../test-utils'
import type { CardCatalog } from '../lib/cards'

// 处理链的渲染契约：
//   1. 四段都出现；2. 点"+"打开能力库并停在该阶段该开的库上；3. 成本提示随卡片数变化。

const catalog: CardCatalog = {
  agents: [{ id: 'a1', name: '导演', role: '拆镜', system: 'x', category: '编剧' }],
  skills: [{ id: 's1', name: '中译英', desc: '翻译', system: 'y', category: '翻译', out_kind: 'text' }],
  workflows: [{ id: '7', name: '成片流水线', nodes: 6, desc: '' }],
  categories: { agent: ['编剧'], skill: ['翻译'], workflow: ['完整流水线（>5 节点）'] },
}

function renderPipe(data: Record<string, unknown> = {}, opts: Partial<{ textIn: boolean; textOut: boolean; bodyCapable: boolean }> = {}) {
  return render(
    <NodePipeline
      data={data}
      cards={catalog}
      upstream={[{ id: 's1', type: 'start' }]}
      downstream={[]}
      textIn={opts.textIn ?? true}
      textOut={opts.textOut ?? true}
      bodyCapable={opts.bodyCapable ?? false}
      onChange={() => {}}
    />,
  )
}

test('四段处理链与接线都在', () => {
  const { container } = renderPipe()
  const text = container.textContent ?? ''
  for (const want of ['接线', '处理链', '输入处理', '本体角色', '输出处理', '整节点替换', '无下游']) {
    assert.ok(text.includes(want), `缺「${want}」`)
  }
})

test('点"+"打开能力库，输入段默认停在技能库', () => {
  const { container, getAllByText } = renderPipe()
  const plus = getAllByText('+')
  // fire 收的是一个函数（内部包 act），不是 (el, event)
  fire(() => plus[0].click())
  const text = container.textContent ?? ''
  assert.ok(text.includes('技能库'), '应打开能力库')
  assert.ok(text.includes('要加到：输入处理'), '应标明加到哪一段')
})

test('贴了卡会显示模型调用次数', () => {
  const { container } = renderPipe({
    bindings: [
      { kind: 'skill', id: 's1', stage: 'input' },
      { kind: 'skill', id: 's1', stage: 'output' },
    ],
  })
  assert.ok((container.textContent ?? '').includes('会调用 2 次模型'), '成本提示缺失')
})

test('不吃文本的节点把输入段置灰并说明原因', () => {
  const { container } = renderPipe({}, { textIn: false })
  assert.ok((container.textContent ?? '').includes('该节点不吃文本'), '缺置灰说明')
})
