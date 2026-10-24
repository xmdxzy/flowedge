import { test } from 'node:test'
import assert from 'node:assert/strict'

import type { PipelineField } from '../api/types'
import {
  buildParams,
  coerceField,
  defaultFieldValue,
  describeBlockers,
  initialForm,
} from './fields'

// 字段值 → 请求体 的转换与校验。
// 语义与 backend/service/pipeline.go 的 coerceField 对齐：前端拦住的错，
// 后端也会拒；前端放过的，后端必须能接受。

const prompt: PipelineField = {
  key: 'prompt',
  label: '画面描述',
  type: 'textarea',
  required: true,
}
const width: PipelineField = {
  key: 'width',
  label: '宽',
  type: 'int',
  default: 1024,
  min: 384,
  max: 1536,
}
const strength: PipelineField = {
  key: 'strength',
  label: '重绘强度',
  type: 'float',
  default: 0.85,
  min: 0.2,
  max: 1,
}
const mode: PipelineField = {
  key: 'mode',
  label: '动作',
  type: 'enum',
  default: 'restyle',
  options: ['restyle', 'inpaint', 'upscale'],
}
const qc: PipelineField = { key: 'qc', label: '质量循环', type: 'bool', default: false }
const imagePath: PipelineField = { key: 'image_path', label: '原图路径', type: 'path' }
/** 不带 min/max 的整数：单看「取整」行为，不掺入越界判定 */
const count: PipelineField = { key: 'count', label: '数量', type: 'int' }

test('必填为空：报 missing、params 里不发这个键、禁用文案点名缺什么', () => {
  const result = buildParams([prompt, width], { prompt: '   ', width: 1024 })
  assert.equal(result.errors.length, 1)
  assert.equal(result.errors[0].missing, true)
  assert.deepEqual(result.missing, ['画面描述'])
  assert.equal('prompt' in result.params, false, '空字符串不能发出去，否则后端会当成已填')
  assert.equal(describeBlockers(result.errors), '还缺必填：画面描述')
})

test('必填填了：去掉首尾空格后发送', () => {
  const result = buildParams([prompt], { prompt: '  雪山日出  ' })
  assert.equal(result.errors.length, 0)
  assert.equal(result.params.prompt, '雪山日出')
})

test('int 取整：12.6 → 13，3.2 → 3（不让后端 400）', () => {
  assert.deepEqual(coerceField(count, 12.6), { value: 13 })
  assert.deepEqual(coerceField(count, '3.2'), { value: 3 })
  assert.deepEqual(coerceField(width, 1024), { value: 1024 })
})

test('int / float 的 min / max 在取整后判定', () => {
  const tooSmall = coerceField(width, 200)
  assert.ok(tooSmall.error)
  assert.match(tooSmall.error!.message, /不能小于 384/)
  assert.equal(tooSmall.value, undefined)

  const tooBig = coerceField(width, 2000)
  assert.match(tooBig.error!.message, /不能大于 1536/)

  // 383.6 四舍五入成 384，正好合法（这是「先取整再判界」的可观测结果）
  assert.deepEqual(coerceField(width, 383.6), { value: 384 })

  const badFloat = coerceField(strength, 1.2)
  assert.match(badFloat.error!.message, /不能大于 1/)
  assert.deepEqual(coerceField(strength, 0.85), { value: 0.85 })
})

test('非数字输入报错，不会发 NaN', () => {
  const bad = coerceField(width, '宽一点')
  assert.ok(bad.error)
  assert.match(bad.error!.message, /要整数/)
  const badFloat = coerceField(strength, '强')
  assert.match(badFloat.error!.message, /要数字/)
})

test('enum 非法值被拒，且提示可选值', () => {
  const bad = coerceField(mode, 'blur')
  assert.ok(bad.error)
  assert.match(bad.error!.message, /只能是 restyle \/ inpaint \/ upscale/)
  assert.deepEqual(coerceField(mode, 'inpaint'), { value: 'inpaint' })
})

test('bool 转成真布尔（字符串 "false" 不能当 true 发出去）', () => {
  assert.deepEqual(coerceField(qc, false), { value: false })
  assert.deepEqual(coerceField(qc, 'false'), { value: false })
  assert.deepEqual(coerceField(qc, true), { value: true })
  assert.ok(coerceField(qc, 'maybe').error)
})

test('非必填空值不发键（让后端合并默认值）', () => {
  const result = buildParams([prompt, width, imagePath], { prompt: '图', width: '', image_path: '' })
  assert.equal(result.errors.length, 0)
  assert.equal('width' in result.params, false)
  assert.equal('image_path' in result.params, false)
  assert.equal(result.params.prompt, '图')
})

test('default 预填：数字/enum/bool 各按类型取值', () => {
  assert.equal(defaultFieldValue(width), 1024)
  assert.equal(defaultFieldValue(strength), 0.85)
  assert.equal(defaultFieldValue(mode), 'restyle')
  assert.equal(defaultFieldValue(qc), false)
  assert.equal(defaultFieldValue(prompt), '')
  // enum 的 default 不在 options 里时退回第一个，不产生非法值
  assert.equal(defaultFieldValue({ ...mode, default: 'nope' }), 'restyle')

  const form = initialForm([prompt, width, mode, qc])
  assert.deepEqual(form, { prompt: '', width: 1024, mode: 'restyle', qc: false })
})

test('整表通过时 params 与字段一一对应，errors 为空', () => {
  const result = buildParams([prompt, width, strength, mode, qc], {
    prompt: '一幅画',
    width: 512,
    strength: 0.5,
    mode: 'upscale',
    qc: true,
  })
  assert.deepEqual(result.errors, [])
  assert.deepEqual(result.params, {
    prompt: '一幅画',
    width: 512,
    strength: 0.5,
    mode: 'upscale',
    qc: true,
  })
  assert.equal(describeBlockers([]), '')
})

test('多条格式错误时说明第一条并给出剩余条数', () => {
  const result = buildParams([width, strength], { width: 10, strength: 9 })
  assert.equal(result.errors.length, 2)
  assert.equal(result.missing.length, 0)
  assert.match(describeBlockers(result.errors), /不能小于 384（另有 1 项不合法）$/)
})
