import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import path from 'node:path'

import { CANVAS_ONLY_TYPES, ENV_LABEL, NODE_OUTPUT_FIELDS, NODE_REQUIRES, PALETTE } from './graph'
import { workflowTemplates } from '../templates/workflowTemplates'

// 前后端契约测试。
//
// 背景（docs/系统评估-2026-10-21.md §9 S4）：前端 9.7k 行零测试，其中最容易腐坏的一环
// 不是算法而是**清单**——后端加了节点、前端调色板没加，能力就等于不存在。
// 2026-10-21 16:15 的盘点就发现过"5 个新能力全部 0 暴露"。
// 这组用例把两边对账，把这类漂移变成 CI 红灯。

/** 从 frontend/ 向上找到仓库根（含 backend/service/nodespec.go 的那一层） */
function findRepoRoot(): string {
  let dir = process.cwd()
  for (let i = 0; i < 6; i++) {
    if (existsSync(path.join(dir, 'backend', 'service', 'nodespec.go'))) return dir
    dir = path.dirname(dir)
  }
  throw new Error(`找不到仓库根（cwd=${process.cwd()}），测试必须在前端目录下运行`)
}

/** 从 Go 的 NodeSpecs 表里抽出节点类型（单一事实源在 backend） */
function backendNodeTypes(): string[] {
  const src = readFileSync(path.join(findRepoRoot(), 'backend', 'service', 'nodespec.go'), 'utf8')
  const re = /"([a-z0-9_]+)"\s*:\s*\{Type:\s*"[a-z0-9_]+"/g
  const out = new Set<string>()
  let m: RegExpExecArray | null
  while ((m = re.exec(src)) !== null) out.add(m[1])
  return Array.from(out).sort()
}

const paletteTypes = PALETTE.flatMap((s) => s.items.map((i) => i.type))

test('后端每个节点类型都在前端调色板里（否则能力等于不存在）', () => {
  const back = backendNodeTypes()
  assert.ok(back.length >= 29, `只解析到 ${back.length} 个类型，解析规则可能过期`)
  const missing = back.filter((t) => !paletteTypes.includes(t))
  assert.deepEqual(missing, [], `这些节点后端能跑但用户点不到：${missing.join(', ')}`)
})

test('调色板里不该有后端不存在的可执行类型（点了必失败）', () => {
  const back = new Set(backendNodeTypes())
  // note / group 是纯画布节点，本来就没有执行体——用户要能拖出来写批注。
  const phantom = paletteTypes.filter((t) => !back.has(t) && !CANVAS_ONLY_TYPES.has(t))
  assert.deepEqual(phantom, [], `调色板有幽灵节点：${phantom.join(', ')}`)
})

test('调色板类型不重复', () => {
  const seen = new Set<string>()
  const dup: string[] = []
  for (const t of paletteTypes) {
    if (seen.has(t)) dup.push(t)
    seen.add(t)
  }
  assert.deepEqual(dup, [], `同一种节点出现在多个分组：${dup.join(', ')}`)
})

test('输出字段表与依赖表只登记调色板里的节点', () => {
  const known = new Set(paletteTypes)
  for (const [t, fields] of Object.entries(NODE_OUTPUT_FIELDS)) {
    assert.ok(known.has(t), `NODE_OUTPUT_FIELDS 里有未知节点 ${t}`)
    assert.ok(fields.length > 0, `${t} 的输出字段为空，下游连线将无字段可选`)
    assert.equal(new Set(fields).size, fields.length, `${t} 的输出字段有重复`)
  }
  for (const t of Object.keys(NODE_REQUIRES)) {
    assert.ok(known.has(t), `NODE_REQUIRES 里有未知节点 ${t}`)
  }
})

test('每个依赖键都必须有中文提示文案（否则用户看到裸键名）', () => {
  const missing = Object.values(NODE_REQUIRES).filter((k) => !(k in ENV_LABEL))
  assert.deepEqual(Array.from(new Set(missing)), [], `缺少提示文案的能力键：${missing.join(', ')}`)
})

test('画布专用类型可以出现在调色板，但不能出现在可执行清单里', () => {
  for (const t of CANVAS_ONLY_TYPES) {
    assert.ok(!(t in NODE_OUTPUT_FIELDS), `${t} 没有执行体，不该有输出字段`)
    assert.ok(!(t in NODE_REQUIRES), `${t} 不连外部服务，不该有依赖声明`)
  }
})

test('预置模板只用已知节点，且连线两端都存在', () => {
  const allowed = new Set([...paletteTypes, ...CANVAS_ONLY_TYPES])
  assert.ok(workflowTemplates.length > 0)
  for (const tpl of workflowTemplates) {
    const ids = new Set(tpl.nodes.map((n: any) => n.id))
    for (const node of tpl.nodes as any[]) {
      assert.ok(allowed.has(node.type), `模板「${tpl.name}」用了未知节点 ${node.type}`)
      assert.ok(node.id, `模板「${tpl.name}」有节点缺 id`)
      assert.ok(ids.has(node.id))
    }
    for (const edge of tpl.edges as any[]) {
      assert.ok(ids.has(edge.source), `模板「${tpl.name}」的连线起点 ${edge.source} 不存在`)
      assert.ok(ids.has(edge.target), `模板「${tpl.name}」的连线终点 ${edge.target} 不存在`)
    }
    assert.equal(new Set(tpl.nodes.map((n: any) => n.id)).size, tpl.nodes.length,
      `模板「${tpl.name}」节点 id 重复`)
  }
})

test('模板 id 唯一（否则模板库会互相覆盖）', () => {
  const ids = workflowTemplates.map((t) => t.id)
  assert.equal(new Set(ids).size, ids.length, '存在重复的模板 id')
})

test('字幕与后期节点必须在调色板里（2026-10-21 交付门槛的回归锁）', () => {
  for (const t of ['subtitles', 'post_production']) {
    assert.ok(paletteTypes.includes(t), `${t} 不在调色板里，用户无法用它出片`)
  }
})
