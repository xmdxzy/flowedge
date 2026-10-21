import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render } from '@testing-library/react'

import { fire, typeInto } from '../test-utils'
import NodePalette from './NodePalette'
import { PALETTE } from '../config/graph'

// 左栏节点库：搜索、常用区、环境未就绪的灰显提示。
// 这三条都是「用户第一眼会碰到」的路径，而且只存在于 DOM 里，纯函数测试覆盖不到。
//
// 所有交互都包在 fire() 里：本环境 RTL 的 fireEvent 不自动包 act，
// 不包的话 setState 只入队不刷新，断言会看到"值变了、列表没变"。

function setup(overrides: Partial<{ envReady: Record<string, boolean> | null; usage: any }> = {}) {
  localStorage.clear()
  const added: string[] = []
  const dragged: string[] = []
  const utils = render(
    <NodePalette
      envReady={overrides.envReady ?? null}
      usage={overrides.usage ?? {}}
      onAdd={(t) => added.push(t)}
      onDragStart={(_e, t) => dragged.push(t)}
    />,
  )
  const search = utils.getByPlaceholderText('搜索节点…') as HTMLInputElement
  const searchFor = (v: string) => typeInto(search, v)
  return { ...utils, added, dragged, search, searchFor }
}

test('搜索按中文名过滤，点一下就把节点加进画布', () => {
  const { searchFor, added, getByText } = setup()
  searchFor('字幕')
  const item = getByText('字幕烧录')
  fire(() => item.click())
  assert.deepEqual(added, ['subtitles'])
})

test('搜索也认 type（英文）', () => {
  const { searchFor, getByText } = setup()
  searchFor('post_production')
  assert.ok(getByText('后期包装'), '按 type 搜不到节点')
})

test('搜不到时给明确空态，而不是一片空白', () => {
  const { searchFor, getByText } = setup()
  searchFor('不存在的节点xyz')
  assert.ok(getByText('没有匹配的节点'))
})

test('环境未就绪的节点要标出来（但仍可先摆好）', () => {
  const { searchFor, getByTitle } = setup({ envReady: { llm: false } })
  searchFor('大语言模型')
  const item = getByTitle(/当前环境未就绪/)
  assert.ok(item.className.includes('opacity-45'), '未就绪节点应有灰显样式')
})

test('环境就绪的节点不带灰显', () => {
  const { searchFor, getByTitle } = setup({ envReady: { llm: true } })
  searchFor('大语言模型')
  const item = getByTitle(/大语言模型/)
  assert.ok(!item.className.includes('opacity-45'))
})

test('有使用记录时出现「常用」区，并按次数排前面', () => {
  const usage = {
    upscale: { count: 9, lastUsed: 2 },
    llm: { count: 3, lastUsed: 1 },
  }
  const { getByText, container } = setup({ usage })
  assert.ok(getByText('常用'))
  // 常用区渲染在分组区之前：使用最多的「超分放大」应早于分组里的「大语言模型」
  const text = container.textContent ?? ''
  const iUp = text.indexOf('超分放大')
  const iLlm = text.indexOf('大语言模型')
  assert.ok(iUp >= 0, '常用区应包含使用过的节点')
  assert.ok(iLlm === -1 || iUp < iLlm, '使用次数多的节点应排在前面')
})

test('折叠状态写进 localStorage（下次打开保持）', () => {
  const { getAllByRole } = setup()
  const groupBtn = getAllByRole('button').find((b) => (b.textContent ?? '').includes(PALETTE[0].label))!
  fire(() => groupBtn.click())
  const saved = localStorage.getItem('flowedge.palette.collapsed.v1')
  assert.ok(saved && saved.includes(PALETTE[0].label), '折叠状态没有落盘')
})
