import assert from 'node:assert/strict'
import test from 'node:test'

import { readPipeline, writePipeline, type Binding } from './cards'

// 旧数据只有一个 binding：读的时候要自动迁到对应阶段，
// 否则老工作流升级后会"贴了卡却不生效"。
test('旧 binding 自动迁移到阶段', () => {
  const p1 = readPipeline({ binding: { kind: 'agent', id: 'a1' } as Binding })
  assert.equal(p1.body?.id, 'a1')

  const p2 = readPipeline({ binding: { kind: 'workflow', id: '9' } as Binding })
  assert.equal(p2.replace?.id, '9')
})

// 三类卡可以同时存在：输入/本体/输出/替换各归各位。
test('多卡共存与顺序', () => {
  const p = readPipeline({
    bindings: [
      { kind: 'skill', id: 'zh2en', stage: 'input', order: 1 },
      { kind: 'skill', id: 'summarize', stage: 'input', order: 0 },
      { kind: 'agent', id: 'prompt-engineer', stage: 'body' },
      { kind: 'skill', id: 'json-extract', stage: 'output' },
      { kind: 'workflow', id: '12', stage: 'replace' },
    ] as Binding[],
  })
  assert.deepEqual(
    p.input.map((b) => b.id),
    ['summarize', 'zh2en'],
  )
  assert.equal(p.body?.id, 'prompt-engineer')
  assert.equal(p.output[0].id, 'json-extract')
  assert.equal(p.replace?.id, '12')
})

// 写回永远是数组 + 重排 order，保证"界面上看到的顺序 = 执行顺序"。
test('写回规整为数组并重排 order', () => {
  const out = writePipeline({
    input: [
      { kind: 'skill', id: 'a', stage: 'input', order: 9 },
      { kind: 'skill', id: 'b', stage: 'input', order: 3 },
    ],
    body: { kind: 'agent', id: 'c', stage: 'body' },
    output: [],
    replace: null,
  })
  assert.deepEqual(
    out.map((b) => b.id),
    ['a', 'b', 'c'],
  )
  assert.equal(out[0].order, 0)
  assert.equal(out[1].order, 1)
  assert.equal(out[2].stage, 'body')
})
