import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render } from '@testing-library/react'
import type { Edge, Node } from '@xyflow/react'

import NodeInspector from './NodeInspector'
import { fire, typeInto } from '../test-utils'

// 属性面板的渲染契约。
//
// 这块是从 App.tsx 抽出来的 1,500 行 JSX（按节点类型分支），最容易在改样式时改坏。
// 抽出它的唯一目的就是让这些断言能存在。

const mk = (type: string, data: Record<string, unknown> = {}, id = 'n1'): Node => ({
  id,
  type,
  position: { x: 0, y: 0 },
  data,
})

const noop = () => {}

function renderPanel(
  node: Node | null,
  opts: Partial<{
    tab: 'config' | 'output'
    nodes: Node[]
    edges: Edge[]
    onUpdate: (k: string, v: unknown) => void
    onNotify: (m: string) => void
    onErrorAction: (a: string) => void
    onTabChange: (t: 'config' | 'output') => void
    savedWorkflows: any[]
  }> = {},
) {
  const updates: Array<[string, unknown]> = []
  const utils = render(
    <NodeInspector
      node={node}
      nodes={opts.nodes ?? (node ? [node] : [])}
      edges={opts.edges ?? []}
      tab={opts.tab ?? 'config'}
      onTabChange={opts.onTabChange ?? noop}
      onUpdate={opts.onUpdate ?? ((k, v) => updates.push([k, v]))}
      onNotify={opts.onNotify}
      onErrorAction={opts.onErrorAction}
      savedWorkflows={opts.savedWorkflows}
    />,
  )
  return { ...utils, updates }
}

test('没选中节点时给指引，而不是空白面板', () => {
  const { container } = renderPanel(null)
  assert.ok((container.textContent ?? '').includes('点击画布中的节点进行配置'))
})

test('开始节点显示提示词输入，改值回调到上层', () => {
  const { getByPlaceholderText, updates } = renderPanel(mk('start', { prompt: '' }))
  const ta = getByPlaceholderText('输入要传递给下游的文本...') as HTMLTextAreaElement
  typeInto(ta, '写一首关于秋天的诗')
  assert.deepEqual(updates, [['prompt', '写一首关于秋天的诗']])
})

test('LLM 节点显示模型与工具选项', () => {
  const { container, getByPlaceholderText } = renderPanel(
    mk('llm', { model: 'qwen2.5:14b', prompt: '总结', tools: [] }),
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('模型'), '缺模型字段标签')
  assert.ok(text.includes('工具调用'), '缺工具选项区')
  assert.equal((getByPlaceholderText('留空则用自动探测到的模型') as HTMLInputElement).value, 'qwen2.5:14b')
})

test('输出 Tab：无输出给说明，有输出渲染 JSON', () => {
  const empty = renderPanel(mk('text2image', {}), { tab: 'output' })
  assert.ok((empty.container.textContent ?? '').includes('暂无输出'))
  empty.unmount()

  const filled = renderPanel(mk('text2image', { output: { image: 'a.png', count: 2 } }), { tab: 'output' })
  const text = filled.container.textContent ?? ''
  assert.ok(text.includes('a.png') && text.includes('count'), `没渲染输出 JSON：${text}`)
})

test('节点报错时把错误码翻成人话，并回传修复动作', () => {
  const actions: string[] = []
  const { container, getByText } = renderPanel(
    mk('tts', { error: "No module named 'ordered_set'", error_code: 'config_missing' }),
    { onErrorAction: (a) => actions.push(a) },
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('依赖服务没起来'), `没有翻译错误码：${text}`)
  fire(() => getByText('打开环境面板').click())
  assert.deepEqual(actions, ['open-env'])
})

test('上游变量可点击复制引用写法（{{id.field}}）', () => {
  const up = mk('text2image', {}, 'up')
  const cur = mk('llm', { prompt: '' }, 'n1')
  const notified: string[] = []
  const { container } = renderPanel(cur, {
    nodes: [up, cur],
    edges: [{ id: 'e', source: 'up', target: 'n1' }],
    onNotify: (m) => notified.push(m),
  })
  const text = container.textContent ?? ''
  assert.ok(text.includes('可用变量'), `没有列出上游变量：${text}`)
  assert.ok(text.includes('{{up.image}}'), `没有生成引用写法：${text}`)
})

test('子工作流节点列出已保存的工作流', () => {
  const { container } = renderPanel(mk('sub_workflow', {}), {
    savedWorkflows: [{ id: 7, name: '出片流水线' }],
  })
  const text = container.textContent ?? ''
  assert.ok(text.includes('出片流水线'), `下拉里没有工作流：${text}`)
})

test('Tab 切换按钮可点（此前是死按钮）', () => {
  const tabs: string[] = []
  const { getByText } = renderPanel(mk('start', {}), { onTabChange: (t) => tabs.push(t) })
  fire(() => getByText('输出').click())
  assert.deepEqual(tabs, ['output'])
})

test('后期节点有响度归一选项，选了目标值会回传数字', () => {
  const updates: Array<[string, unknown]> = []
  const { container } = renderPanel(mk('post_production', { loudness: 0 }), {
    onUpdate: (k, v) => updates.push([k, v]),
  })
  assert.ok((container.textContent ?? '').includes('响度归一'), '缺响度归一字段')
  // 面板里还有「构图 / 填充方式」两个下拉，按选项文本挑出响度那个
  const select = Array.from(container.querySelectorAll('select')).find((el) =>
    Array.from(el.options).some((o) => o.value === '-14'),
  ) as HTMLSelectElement | undefined
  assert.ok(select, '没找到响度下拉')
  typeInto(select, '-14')
  assert.deepEqual(updates, [['loudness', -14]], '选项没回传成 LUFS 数字')
})
