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
      agents={opts.agents}
      onReloadAgents={opts.onReloadAgents}
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

test('LLM 节点：模型是下拉、任务是预设、工具带说明', () => {
  const { container } = renderPanel(
    mk('llm', { model: 'qwen2.5:14b', prompt: '总结', tools: [] }),
  )
  const text = container.textContent ?? ''
  assert.ok(text.includes('模型'), '缺模型字段标签')
  assert.ok(text.includes('处理链'), '缺处理链（工具已并入本体段）')
  // 模型从自由文本框改成下拉：能回显当前模型，且首项是"自动探测"
  // 注：本 harness 的 getByDisplayValue 对 <select> 不生效，直接用 DOM 断言。
  const models = container.querySelector('select') as HTMLSelectElement
  assert.equal(models.value, 'qwen2.5:14b', '模型下拉没回显当前值')
  assert.equal(models.options[0].value, '', '首项应为"自动探测"（空值）')
  assert.ok(models.textContent?.includes('跟随引擎默认'), '下拉缺"跟随引擎默认"项')
  // 工具并进处理链的本体段：默认折叠成一行计数，展开后每项都写清什么时候被调用
  assert.ok(text.includes('工具 0/4'), '缺工具折叠入口')
  const toolRow = Array.from(container.querySelectorAll('button')).find((b) =>
    (b.textContent || '').includes('工具 0/4'),
  )
  assert.ok(toolRow, '找不到工具折叠按钮')
  fire(() => toolRow!.click())
  const expanded = container.textContent ?? ''
  assert.ok(
    expanded.includes('知识库检索') && expanded.includes('单位换算'),
    '展开后应看到每个工具的调用时机',
  )
  // 预设：prompt 与预设不匹配时归到"自定义"，仍能看到自定义输入框
  assert.ok(expanded.includes('最多轮数') && expanded.includes('工具失败即中止'), '缺工具轮数/严格开关')
})

test('节点面板有接线与三段处理链', () => {
  const { container } = renderPanel(mk('llm', {}))
  const text = container.textContent ?? ''
  for (const want of ['接线', '处理链', '输入处理', '本体角色', '输出处理', '整节点替换']) {
    assert.ok(text.includes(want), `缺「${want}」`)
  }
})

test('开始节点只有接线与提示词，没有处理链', () => {
  const { container } = renderPanel(mk('start', { prompt: '写个开场' }))
  const text = container.textContent ?? ''
  assert.ok(text.includes('接线'), '开始节点要有接线')
  assert.ok(text.includes('手动运行'), '开始节点要有外部源选择')
  assert.ok(text.includes('输入提示词'), '开始节点要有提示词')
  // 处理链对开始节点没有落点：不该出现四段
  for (const absent of ['处理链', '整节点替换', '本体角色']) {
    assert.ok(!text.includes(absent), `开始节点不该出现「${absent}」`)
  }
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

// ===== 外部智能体（agent_task）=====

test('外部智能体节点：必须警示"不承诺可复现"，并列出名册', () => {
  const { container, updates } = renderPanel(mk('agent_task', { agent_id: 'claude-code' }), {
    agents: [
      { id: 'claude-code', label: 'Claude Code', kind: 'cli', available: true, detail: 'C:/bin/claude.exe' },
      { id: 'codex', label: 'Codex', kind: 'cli', enabled: false, available: false, detail: '已停用' },
    ],
  })
  const text = container.textContent ?? ''
  // 这条警告是功能性的：用户不知道它不可复现，就会拿它去做本该确定性的活儿
  assert.ok(text.includes('不承诺可复现'), '缺非确定性警示')
  assert.ok(text.includes('评测时本图要与确定性图分开统计'), '缺评测分组提示')

  const select = Array.from(container.querySelectorAll('select')).find((el) =>
    Array.from(el.options).some((o) => o.value === 'claude-code'),
  ) as HTMLSelectElement | undefined
  assert.ok(select, '没找到智能体下拉')
  assert.equal(select.value, 'claude-code', '下拉没回显当前 agent_id')
  assert.ok(select.textContent?.includes('可用'), '选项里要标出可用性')

  // 停用的不该出现在下拉里（但已在节点上配好的仍要能显示状态）
  assert.ok(!select.textContent?.includes('Codex'), '停用的智能体不该进下拉')
  assert.ok(text.includes('C:/bin/claude.exe'), '选中项要显示探活详情（不只是"可用"两个字）')

  typeInto(select, 'codex')
  assert.deepEqual(updates, [['agent_id', 'codex']])
})

test('名册为空时给配置指引，而不是一个空下拉', () => {
  const { container } = renderPanel(mk('agent_task', {}), { agents: [] })
  const text = container.textContent ?? ''
  assert.ok(text.includes('data/agents.json'), '要指出配置位置')
  assert.ok(text.includes('停用'), '要说明内置示例默认停用（否则用户以为坏了）')
})

test('已配但不可用的智能体：显示真实原因，不假装可用', () => {
  const { container } = renderPanel(mk('agent_task', { agent_id: 'ghost' }), {
    agents: [
      { id: 'ghost', label: 'Ghost', kind: 'cli', available: false, detail: 'PATH 里找不到命令 "ghost"' },
    ],
  })
  const text = container.textContent ?? ''
  assert.ok(text.includes('找不到命令'), '要把探活失败的原因摊开，而不是只标红')
})

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
