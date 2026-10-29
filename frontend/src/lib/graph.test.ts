import { test } from 'node:test'
import assert from 'node:assert/strict'

import type { CatalogNode, GraphReport, GraphTemplate } from '../api/graphTypes'
import {
  canConnect,
  catalogMap,
  connectRejection,
  costliestLines,
  defaultDataFor,
  draftFromTemplate,
  formatEstimate,
  humanDuration,
  issueIndex,
  kindsCompatible,
  nextNodeId,
  summarizeReport,
} from './graph'

function cat(type: string, inKinds: string[], outKinds: string[]): CatalogNode {
  return {
    type,
    label: type,
    side: 'diffusion',
    heavy: true,
    media: true,
    in: inKinds.map((k) => ({ key: k, kind: k as never })),
    out: outKinds.map((k) => ({ key: k, kind: k as never })),
    fields: [{ key: 'width', label: '宽', kind: 'int', default: 1024 }],
    cost: {
      gpu_class: 'diffusion',
      base_sec: 15,
      cold_load_sec: 45,
      sec_per_unit: 30,
      unit: 'image',
      vram_mb: 11000,
    },
  }
}

const CATALOG = catalogMap([
  cat('start', [], ['text']),
  cat('text2image', ['text'], ['image']),
  cat('upscale', ['image'], ['image']),
  cat('end', [], ['any']),
])

test('端口兼容：any 兼容一切、任一侧无端口视为兼容', () => {
  assert.equal(kindsCompatible(['text'], ['text']), true)
  assert.equal(kindsCompatible(['text'], ['any']), true)
  assert.equal(kindsCompatible([], ['image']), true)
  assert.equal(kindsCompatible(['text'], ['image']), false)
})

test('连线前先拦：text → upscale 连不上，且给出可照抄的理由', () => {
  assert.equal(canConnect(CATALOG, 'start', 'text2image'), true)
  assert.equal(canConnect(CATALOG, 'start', 'upscale'), false)
  assert.equal(canConnect(CATALOG, 'nope', 'upscale'), false)
  const why = connectRejection(CATALOG, 'start', 'upscale')
  assert.match(why, /text/)
  assert.match(why, /image/)
  assert.match(why, /不做隐式转换/)
})

test('时长念法：秒 / 分 / 小时三档', () => {
  assert.equal(humanDuration(45), '45 秒')
  assert.equal(humanDuration(90), '1.5 分钟')
  assert.equal(humanDuration(1800), '30 分钟')
  assert.equal(humanDuration(7200), '2.0 小时')
})

test('估算必须念成区间，不许给单值（AGENTS §0.3）', () => {
  const est = formatEstimate({
    optimistic_sec: 60,
    p50_sec: 100,
    p90_sec: 180,
    worst_sec: 900,
    cold_load_sec: 90,
    vram_peak_mb: 23800,
    lines: [],
    uncertain: [],
    caveat: '区间估算',
  })
  assert.match(est.main, /–/)
  assert.match(est.detail, /P50/)
  assert.match(est.detail, /上限/)
  assert.match(est.detail, /峰值显存 23\.2 GB/)
})

test('体检报告摊到节点与连线上', () => {
  const report: GraphReport = {
    ok: false,
    node_count: 2,
    edge_count: 1,
    blocking: 1,
    warnings: 1,
    dag: [{ code: 'orphan_node', level: 'warn', message: '节点没有任何连线', nodes: ['b'] }],
    ports: [
      {
        code: 'type_mismatch',
        level: 'warn',
        message: '这条线不会被使用',
        nodes: ['a', 'b'],
        edge_id: 'e1',
      },
      {
        code: 'input_unreachable',
        level: 'error',
        message: 'b 的图像输入没有来源',
        nodes: ['b'],
      },
    ],
    params: [{ node_id: 'a', node_type: 'x', field: 'prompt', message: '缺少提示词' }],
  }
  const { byNode, byEdge } = issueIndex(report)
  assert.deepEqual(byNode.get('b')?.errors, ['b 的图像输入没有来源'])
  assert.equal(byNode.get('b')?.warns.length, 2)
  assert.deepEqual(byNode.get('a')?.errors, ['缺少提示词'])
  assert.deepEqual(byEdge.get('e1')?.warns, ['这条线不会被使用'])
})

test('体检一句话：绿的时候说能保存', () => {
  assert.equal(
    summarizeReport({
      ok: true,
      node_count: 3,
      edge_count: 2,
      blocking: 0,
      warnings: 0,
      dag: [],
      ports: [],
      params: [],
    }),
    '可以保存：3 个节点 / 2 条连线',
  )
  assert.equal(summarizeReport(null), '还没体检')
})

test('模板草稿：缺坐标时补网格，不堆在原点', () => {
  const tpl: GraphTemplate = {
    pipeline_id: 'image',
    name: '文生图',
    summary: '',
    output: 'image',
    nodes: [
      { id: 'a', type: 'start', data: {} },
      { id: 'b', type: 'text2image', data: { width: 512 } },
    ],
    edges: [{ id: 'e1', source: 'a', target: 'b' }],
  }
  const draft = draftFromTemplate(tpl)
  assert.equal(draft.nodes.length, 2)
  for (const n of draft.nodes) {
    assert.ok(n.position)
    assert.notEqual(`${n.position?.x},${n.position?.y}`, '0,0')
  }
  // 服务端给的坐标不许被覆盖
  const withPos = draftFromTemplate({
    ...tpl,
    nodes: [{ id: 'a', type: 'start', position: { x: 500, y: 300 }, data: {} }],
  })
  assert.deepEqual(withPos.nodes[0].position, { x: 500, y: 300 })
})

test('新节点 id 不撞车，默认值只来自 catalog', () => {
  assert.equal(nextNodeId('text2image', []), 'text2image-1')
  assert.equal(nextNodeId('text2image', [{ id: 'text2image-1' }]), 'text2image-2')
  assert.deepEqual(defaultDataFor(CATALOG.get('text2image')!), { width: 1024 })
})

test('谁最贵：按耗时降序，缺 lines 时返回空数组而不是崩', () => {
  const est = {
    optimistic_sec: 1,
    p50_sec: 1,
    p90_sec: 1,
    worst_sec: 1,
    cold_load_sec: 0,
    vram_peak_mb: 0,
    uncertain: [],
    caveat: '',
    lines: [
      { node_id: 'a', node_type: 'x', unit: 'image', count: 1, area_mul: 1, seconds: 10 },
      { node_id: 'b', node_type: 'y', unit: 'frame', count: 1, area_mul: 1, seconds: 99 },
    ],
  }
  assert.deepEqual(
    costliestLines(est).map((l) => l.node_id),
    ['b', 'a'],
  )
  assert.deepEqual(costliestLines(null), [])
})
