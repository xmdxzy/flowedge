import { test } from 'node:test'
import assert from 'node:assert/strict'

import { getDefaultNodeData } from './nodeDefaults'
import { PALETTE } from './graph'

// 默认数据是所有节点面板的起点：字段缺了，面板上就是一堆 undefined，
// 用户得先想"我该填什么"才能让节点跑起来。

const paletteTypes = PALETTE.flatMap((s) => s.items.map((i) => i.type))

test('调色板里每种节点都有默认 data', () => {
  for (const type of paletteTypes) {
    const data = getDefaultNodeData(type)
    assert.equal(typeof data, 'object', `${type} 没有默认 data`)
    assert.notEqual(data, null)
  }
})

test('未知类型返回空对象而不是崩溃或 undefined', () => {
  const data = getDefaultNodeData('__no_such_type__')
  assert.deepEqual(data, {})
})

test('默认值里不能出现 undefined（面板会显示成空、且提交时被 JSON 丢掉）', () => {
  // 唯一例外：workflow_id。它表示"还没选子工作流"，UI 用 undefined 判定未选、
  // JSON 序列化时自然省略，后端按 0 处理——这是有意为之，不是漏填。
  const allowed = new Set(['workflow_id'])
  for (const type of paletteTypes) {
    const data = getDefaultNodeData(type)
    for (const [k, v] of Object.entries(data)) {
      if (allowed.has(k)) continue
      assert.notEqual(v, undefined, `${type}.${k} 的默认值是 undefined`)
    }
  }
})

test('关键节点的默认值要能直接跑（而不是全空字段）', () => {
  const llm = getDefaultNodeData('llm')
  assert.ok(typeof llm.model === 'string' && llm.model.length > 0, 'llm 应带一个默认模型')
  assert.ok(Array.isArray(llm.tools), 'llm.tools 应是数组，否则工具面板会崩')

  const t2i = getDefaultNodeData('text2image')
  assert.ok(t2i.width > 0 && t2i.height > 0, '文生图应带正数分辨率')
  assert.ok(t2i.batch >= 1, 'batch 至少 1')

  const sub = getDefaultNodeData('subtitles')
  for (const k of ['video_path', 'srt_path', 'plan', 'dialogues', 'font_size', 'margin_v']) {
    assert.ok(k in sub, `subtitles 缺字段 ${k}`)
  }

  const post = getDefaultNodeData('post_production')
  assert.equal(post.aspect, '9:16', '后期默认竖屏（短视频主场景）')
  assert.equal(post.fit, 'blur', '默认模糊垫底：横屏改竖屏不丢内容')
  assert.equal(post.duck, true, '人声压 BGM 默认开')
  assert.equal(post.bgm_gain, -12)
})

test('布尔型默认值必须是真布尔（面板用 !== false 判定，字符串 "false" 会误判）', () => {
  for (const type of paletteTypes) {
    const data = getDefaultNodeData(type)
    for (const [k, v] of Object.entries(data)) {
      if (typeof v === 'string' && (v === 'true' || v === 'false')) {
        assert.fail(`${type}.${k} 用了字符串 "${v}" 表示布尔，应该用真正的 boolean`)
      }
    }
  }
})
