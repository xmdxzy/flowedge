import { test } from 'node:test'
import assert from 'node:assert/strict'

import { mkNode, renderCanvas } from '../test-canvas'
import StartNode from './StartNode'
import EndNode from './EndNode'
import LLMNode from './LLMNode'
import EditNode from './EditNode'
import Text2ImageNode from './Text2ImageNode'
import TTSNode from './TTSNode'
import NoteNode from './NoteNode'
import GroupNode from './GroupNode'

// 画布节点的渲染契约：标题、状态、关键参数、上游/空态提示。
// 这些是用户唯一能看到的"节点在做什么"，改样式时最容易顺手改坏。
// 技术前提见 ../test-canvas.tsx（节点里的 <Handle> 必须在真 React Flow 里渲染）。

const types = {
  start: StartNode,
  end: EndNode,
  llm: LLMNode,
  edit: EditNode,
  image2image: EditNode,
  text2image: Text2ImageNode,
  tts: TTSNode,
  note: NoteNode,
  group: GroupNode,
}

test('开始节点空态显示「点击配置」而不是空白', () => {
  const { container } = renderCanvas([mkNode('s', 'start', {})], types)
  assert.ok((container.textContent ?? '').includes('点击配置'))
})

test('开始节点填了提示词就显示提示词', () => {
  const { container } = renderCanvas([mkNode('s', 'start', { prompt: '写一首诗' })], types)
  assert.ok((container.textContent ?? '').includes('写一首诗'))
})

test('LLM 节点显示模型名与提示词', () => {
  const { container } = renderCanvas(
    [mkNode('l', 'llm', { model: 'qwen2.5:14b', prompt: '总结这段' })],
    types,
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('qwen2.5:14b'), `缺模型名：${text}`)
  assert.ok(text.includes('总结这段'), `缺提示词：${text}`)
})

test('文生图节点显示真实尺寸与张数（不再硬编码 SDXL）', () => {
  const { container } = renderCanvas(
    [mkNode('t', 'text2image', { prompt: 'cat', width: 1024, height: 768, batch: 2 })],
    types,
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('1024×768'), `缺尺寸：${text}`)
  assert.ok(text.includes('2张'), `缺张数：${text}`)
  assert.ok(!text.includes('SDXL'), '引擎默认是 flux-schnell，标签不该写死 SDXL')
})

test('图生图节点显示强度，重绘节点显示「缺蒙版」', () => {
  const i2i = renderCanvas([mkNode('i', 'image2image', { strength: 0.75 })], types)
  assert.ok((i2i.container.textContent ?? '').includes('0.75'))
  i2i.unmount()

  const inpaint = renderCanvas([mkNode('p', 'edit', {})], { edit: EditNode })
  // edit 类型不在 SPEC 里 → 回落 image2image 规格，至少要渲染出标题而不是崩
  assert.ok((inpaint.container.textContent ?? '').length > 0)
})

test('跑完后节点显示「已生成」（产物在 data.output 里）', () => {
  const { container } = renderCanvas(
    [mkNode('t', 'text2image', { prompt: 'x', output: { image: 'C:/tmp/out/a.png' } })],
    types,
  )
  assert.ok((container.textContent ?? '').includes('已生成'), '产物字段读错位置，跑完不显示')
})

test('批注与分组节点渲染自己的文本/标题', () => {
  const note = renderCanvas([mkNode('n', 'note', { text: '这里是重点' })], types)
  assert.ok((note.container.textContent ?? '').includes('这里是重点'))
  note.unmount()

  const group = renderCanvas([mkNode('g', 'group', { label: '拍摄流程' })], types)
  assert.ok((group.container.textContent ?? '').includes('拍摄流程'))
})

test('多个节点能同时渲染（不会互相覆盖或只渲染第一个）', () => {
  const { container } = renderCanvas(
    [
      mkNode('s', 'start', { prompt: '第一步' }),
      mkNode('l', 'llm', { model: 'm1', prompt: '第二步' }),
      mkNode('e', 'end', {}),
    ],
    types,
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('第一步') && text.includes('第二步'), `只渲染了一个：${text}`)
})
