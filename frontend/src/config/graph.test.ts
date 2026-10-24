import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import path from 'node:path'

import { CANVAS_ONLY_TYPES, ENV_LABEL, NODE_OUTPUT_FIELDS, NODE_REQUIRES, PALETTE } from './graph'
import { SOURCE_EXT_KINDS, isPlaceholderPath } from '../lib/inputSource'
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
  const out = new Set<string>()
  // 两种写法都要认：
  //   单行  "llm": {Type: "llm"},
  //   多行  "agent_task": {\n\t\tType: "agent_task", …   ← 带 Timeout 闭包时必然多行
  // 只认单行会让"加一个带闭包的节点"直接破测试，而那不是节点的错。
  const single = /"([a-z0-9_]+)"\s*:\s*\{Type:\s*"[a-z0-9_]+"/g
  let m: RegExpExecArray | null
  while ((m = single.exec(src)) !== null) out.add(m[1])
  const multi = /"([a-z0-9_]+)"\s*:\s*\{\s*\r?\n\s*Type:\s*"([a-z0-9_]+)"/g
  while ((m = multi.exec(src)) !== null) out.add(m[1])
  return Array.from(out).sort()
}

const paletteTypes = PALETTE.flatMap((s) => s.items.map((i) => i.type))

test('后端每个节点类型都在前端调色板里（否则能力等于不存在）', () => {
  const back = backendNodeTypes()
  assert.ok(back.length >= 20, `只解析到 ${back.length} 个类型，解析规则可能过期`)
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

// 文件源的扩展名→类型表在前端也有一份（为了即时反馈：填了 .mp3 却选「图片」时不用等运行）。
// 两份表会漂，所以这里直接去解析后端源码对账。
//
// 为什么不去读契约：契约只有四个端口，没有"哪些扩展名算音频"这层信息 ——
// 那层信息在后端 `sourceExtKinds` 里，只能解析源码。
test('文件源扩展名表前后端一致（前端即时反馈不能与后端判定分叉）', () => {
  const src = readFileSync(path.join(findRepoRoot(), 'backend', 'service', 'input_source.go'), 'utf8')
  const body = src.slice(src.indexOf('var sourceExtKinds'), src.indexOf('}', src.indexOf('var sourceExtKinds')))

  const backend: Record<string, string> = {}
  const re = /"(\.\w+)"\s*:\s*sourceKind(Image|Audio|Video|Text)/g
  let m: RegExpExecArray | null
  while ((m = re.exec(body)) !== null) backend[m[1]] = m[2].toLowerCase()

  assert.ok(Object.keys(backend).length >= 15, `后端扩展名表只解析出 ${Object.keys(backend).length} 条，解析规则可能过期`)

  const front = SOURCE_EXT_KINDS
  const onlyBackend = Object.keys(backend).filter((k) => !(k in front))
  const onlyFront = Object.keys(front).filter((k) => !(k in backend))
  assert.deepEqual(onlyBackend, [], '这些扩展名后端认、前端不认（前端会误报「不匹配」）')
  assert.deepEqual(onlyFront, [], '这些扩展名前端认、后端不认（前端会放行、运行时报错）')

  for (const [ext, kind] of Object.entries(backend)) {
    assert.equal(front[ext], kind, `${ext} 的类型两边不一致`)
  }
})

// 占位判定也要一致：前端说"还是占位值"的，后端必须也认为是占位（而不是报"路径不存在"）
test('文件源占位值判定前后端一致', () => {
  const src = readFileSync(path.join(findRepoRoot(), 'backend', 'service', 'input_source.go'), 'utf8')
  const line = src.slice(src.indexOf('var sourcePlaceholders'), src.indexOf('}', src.indexOf('var sourcePlaceholders')))
  const backendPlaceholders = Array.from(line.matchAll(/"([^"]+)"/g)).map((x) => x[1])

  assert.ok(backendPlaceholders.length >= 5, '占位值表解析失败')
  for (const ph of backendPlaceholders) {
    // 「待提供」是我们在提示词与文档里统一用的那个写法，必须被前端认出来
    assert.equal(isPlaceholderPath(ph), true, `前端没把后端占位值 ${ph} 当占位`)
  }
  // 反向：正常路径不能被误判成占位
  assert.equal(isPlaceholderPath('D:/素材/录音.wav'), false)
  assert.equal(isPlaceholderPath('/home/u/a.mp4'), false)
})
