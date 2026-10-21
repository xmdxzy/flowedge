import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { Edge, Node } from '@xyflow/react'

import { autoLayout, boundingBox, nextId, stripCanvasOnlyEdges, stripCanvasOnlyNodes } from './graphUtils'

const n = (id: string, type: string, x = 0, y = 0, extra: Record<string, unknown> = {}): Node => ({
  id,
  type,
  position: { x, y },
  data: {},
  ...extra,
})

const e = (source: string, target: string): Edge => ({ id: `${source}-${target}`, source, target })

test('stripCanvasOnlyNodes 剔除批注/分组，并去掉纯 UI 字段', () => {
  const nodes = [
    n('a', 'llm', 0, 0, { width: 200, height: 90, selected: true, dragging: true }),
    n('note-1', 'note'),
    n('group-1', 'group'),
  ]
  const out = stripCanvasOnlyNodes(nodes)
  assert.equal(out.length, 1)
  assert.equal(out[0].id, 'a')
  assert.equal('width' in out[0], false, 'width 是画布测量值，不该提交给引擎')
  assert.equal('selected' in out[0], false)
  assert.equal('dragging' in out[0], false)
})

test('stripCanvasOnlyEdges 丢掉指向画布节点的连线', () => {
  const nodes = [n('a', 'llm'), n('b', 'end'), n('note-1', 'note')]
  const edges = [e('a', 'b'), e('a', 'note-1'), e('note-1', 'b')]
  const out = stripCanvasOnlyEdges(nodes, edges)
  assert.deepEqual(out.map((x) => x.id), ['a-b'])
})

test('autoLayout 按拓扑分层，同层纵向排开', () => {
  const nodes = [n('a', 'start'), n('b', 'llm'), n('c', 'end'), n('d', 'llm')]
  const edges = [e('a', 'b'), e('b', 'c'), e('a', 'd')]
  const out = autoLayout(nodes, edges, { layerGap: 300, nodeGap: 150, originX: 80, originY: 120 })
  const pos = Object.fromEntries(out.map((x) => [x.id, x.position]))
  assert.deepEqual(pos.a, { x: 80, y: 120 })
  assert.deepEqual(pos.b, { x: 380, y: 120 })
  assert.deepEqual(pos.d, { x: 380, y: 270 }, 'b 与 d 同层，第二个要往下排')
  assert.deepEqual(pos.c, { x: 680, y: 120 })
})

test('autoLayout 取最长路径而不是最短：汇合节点等所有上游', () => {
  // a→b→c→d 与 a→d 同时存在，d 必须排在 c 之后
  const nodes = [n('a', 'start'), n('b', 'llm'), n('c', 'llm'), n('d', 'end')]
  const edges = [e('a', 'b'), e('b', 'c'), e('c', 'd'), e('a', 'd')]
  const pos = Object.fromEntries(autoLayout(nodes, edges).map((x) => [x.id, x.position]))
  assert.ok(pos.d.x > pos.c.x, `d 应在 c 右侧，实际 d=${pos.d.x} c=${pos.c.x}`)
})

test('autoLayout 不会丢掉环内节点，也不会让它们堆在原点', () => {
  const nodes = [n('a', 'llm'), n('b', 'llm'), n('orbit', 'llm')]
  // a、b 自相连接成环 + 一个孤立节点
  const edges = [e('a', 'b'), e('b', 'a')]
  const out = autoLayout(nodes, edges)
  const ids = out.map((x) => x.id).sort()
  assert.deepEqual(ids, ['a', 'b', 'orbit'], '节点一个都不能少')
  const orbit = out.find((x) => x.id === 'orbit')!
  assert.ok(orbit.position.x > 0, '环内/孤立节点要排到最后一层，不能堆在原点')
})

test('autoLayout 不动批注/分组的位置', () => {
  const nodes = [n('a', 'start'), n('note-1', 'note', 999, 888)]
  const out = autoLayout(nodes, [])
  const note = out.find((x) => x.id === 'note-1')!
  assert.deepEqual(note.position, { x: 999, y: 888 }, '用户摆的位置就是语义，布局不该覆盖')
})

test('boundingBox 含 padding 与标题条，空集合返回 null', () => {
  assert.equal(boundingBox([], ['x']), null)
  const nodes = [n('a', 'llm', 100, 200, { width: 200, height: 100 })]
  const box = boundingBox(nodes, ['a'], 10)!
  assert.equal(box.x, 90)
  assert.equal(box.y, 200 - 10 - 28)
  assert.equal(box.width, 200 + 20)
  assert.equal(box.height, 100 + 20 + 28)
})

test('boundingBox 缺测量值时退回默认尺寸（不能算出 NaN）', () => {
  const nodes = [n('a', 'llm', 0, 0)]
  const box = boundingBox(nodes, ['a'], 0)!
  assert.ok(Number.isFinite(box.width) && box.width > 0)
  assert.ok(Number.isFinite(box.height) && box.height > 0)
})

test('nextId 跳过已占用的编号', () => {
  assert.equal(nextId('node', new Set()), 'node-1')
  assert.equal(nextId('node', new Set(['node-1', 'node-2'])), 'node-3')
})
