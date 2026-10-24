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
    agents: any[]
    onReloadAgents: () => void
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

test('循环节点列出可引用的工作流', () => {
  const { container } = renderPanel(mk('loop', {}), {
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

// ===== 外部智能体（agent_task）=====




// ===== 文件源（input_source）=====

test('文件源：显示「实际会输出哪个端口」和「能接什么」', () => {
  const { container } = renderPanel(mk('input_source', { kind: 'audio', path: 'D:/素材/录音.wav' }))
  const text = container.textContent ?? ''
  // 这两句是这层面板存在的理由：四个端口互斥，不说清就会连错
  assert.ok(text.includes('只输出所选类型的那一个'), '要说明端口互斥')
  assert.ok(text.includes('audio'), '要显示实际输出端口名')
  assert.ok(text.includes('语音转写'), '要提示下游能接什么')
})

test('文件源：不选类型时按扩展名推断，并说明是推断的', () => {
  const { container } = renderPanel(mk('input_source', { path: 'D:/素材/片子.mp4' }))
  const text = container.textContent ?? ''
  assert.ok(text.includes('按扩展名 .mp4 推断'), '要标明是推断，别让人以为是自己选的')
  assert.ok(text.includes('竖屏') || text.includes('后期'), '视频要提示可接后期')
})

test('文件源：占位值与「填错了」必须给出不同的提示', () => {
  const a = renderPanel(mk('input_source', { kind: 'video', path: '待提供' }))
  assert.ok((a.container.textContent ?? '').includes('还是占位值'), '占位值要提示可以稍后补，而不是报错')

  const b = renderPanel(mk('input_source', { kind: 'image', path: 'D:/素材/录音.wav' }))
  const t = b.container.textContent ?? ''
  assert.ok(t.includes('与所选'), '类型与扩展名冲突要在面板上直接说')
  assert.ok(t.includes('.wav'), '要指出是哪个扩展名的问题')
})

test('文件源：选文本类型时不显示文件路径输入', () => {
  const { container } = renderPanel(mk('input_source', { kind: 'text' }))
  const text = container.textContent ?? ''
  assert.ok(text.includes('文本内容'), 'kind=text 要给文本域')
  assert.ok(!text.includes('文件路径'), 'kind=text 不该再要求路径（那是多余的必填项）')
})

test('文件源：改类型会往上层回写 kind', () => {
  const { container, updates } = renderPanel(mk('input_source', { kind: 'image', path: 'D:/a.png' }))
  const select = Array.from(container.querySelectorAll('select')).find((el) =>
    Array.from(el.options).some((o) => o.value === 'audio'),
  ) as HTMLSelectElement | undefined
  assert.ok(select, '没找到类型下拉')
  assert.equal(select.value, 'image', '下拉没回显当前 kind')
  typeInto(select, 'audio')
  assert.deepEqual(updates, [['kind', 'audio']])
})

// ===== 文本拆分（text_split）=====

test('文本拆分：默认按句子，并解释每种方式的行为', () => {
  const { container } = renderPanel(mk('text_split', {}))
  const text = container.textContent ?? ''
  assert.ok(text.includes('按句子'), '要有拆分方式下拉')
  assert.ok(text.includes('每句一段'), '要说明默认方式的效果，而不是只给一个枚举值')
})

test('文本拆分：段数与字数上限只在对应模式下出现', () => {
  const countMode = renderPanel(mk('text_split', { mode: 'by_count', count: 3 }))
  const ct = countMode.container.textContent ?? ''
  assert.ok(ct.includes('段数'), 'by_count 要能填段数')
  assert.ok(!ct.includes('每段字数上限'), 'by_count 不该出现字数上限（那是 by_length 的参数）')

  const lenMode = renderPanel(mk('text_split', { mode: 'by_length', max_chars: 80 }))
  const lt = lenMode.container.textContent ?? ''
  assert.ok(lt.includes('每段字数上限'), 'by_length 要能填字数上限')
  assert.ok(!lt.includes('句子数少于段数'), 'by_length 不该显示段数说明')
})

test('文本拆分：显示字数，并说明三个输出端口的用途', () => {
  const { container } = renderPanel(mk('text_split', { text: '一二三四五' }))
  const text = container.textContent ?? ''
  assert.ok(text.includes('5 字'), '要显示当前字数（判断该按字数还是按句拆）')
  assert.ok(text.includes('parts'), '要指出 parts 能给 loop（否则没人知道怎么逐段处理）')
  assert.ok(text.includes('确定性'), '要说明切分是确定性的（评测可复现的前提）')
})

test('文本拆分：改方式会回写 mode', () => {
  const { container, updates } = renderPanel(mk('text_split', {}))
  const select = Array.from(container.querySelectorAll('select')).find((el) =>
    Array.from(el.options).some((o) => o.value === 'by_paragraph'),
  ) as HTMLSelectElement | undefined
  assert.ok(select, '没找到拆分方式下拉')
  typeInto(select, 'by_paragraph')
  assert.deepEqual(updates, [['mode', 'by_paragraph']])
})

// 质量循环：第二件核心能力的入口必须在面板上看得见、改得动。
// 缺了这块，用户只能去 JSON 里手写 max_rounds/target_score——等于核心能力不可用。
test('质量循环：轮次/目标分/修复策略可配，且改值回传', () => {
  const { container, getByText, updates } = renderPanel(
    mk('quality_loop', { workflow_id: 3, max_rounds: 3, target_score: 100, repair: 'auto' }),
    { savedWorkflows: [{ id: 3, name: '出片主链' }] },
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('最多轮次'), '缺「最多轮次」')
  assert.ok(text.includes('目标分'), '缺「目标分」')
  assert.ok(text.includes('修复策略'), '缺修复策略选择')
  assert.ok(text.includes('达到目标分即停'), '没有解释循环何时停')

  // 目标分输入：改成 80 要回传数字（不是字符串）
  const nums = Array.from(container.querySelectorAll('input[type=number]')) as HTMLInputElement[]
  const target = nums.find((n) => n.value === '100')
  assert.ok(target, '找不到目标分输入')
  typeInto(target!, '80')
  assert.ok(
    updates.some(([k, v]) => k === 'target_score' && v === 80),
    `target_score 未回传数字：${JSON.stringify(updates)}`,
  )
  void getByText
})
