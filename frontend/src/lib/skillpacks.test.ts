import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  humanBytes,
  sortPacks,
  tierLabel,
  tierTone,
  verdictLabel,
  verdictTone,
  type SkillPack,
  type SkillTier,
  type SkillVerdict,
} from './skillpacks'

// 这些函数是面板的全部判断逻辑。它们错了，界面会用错误的语气报告结论——
// 比如把 mirrored 显示成"已收录"，用户就以为可以贴卡了；
// 或者把 domain 当成"被删掉"，用户就以为平台用不了某领域的经验。

const ALL_VERDICTS: SkillVerdict[] = [
  'mirrored',
  'adopted',
  'partial',
  'harness',
  'project',
  'deprecated',
  'unclassified',
]

const ALL_TIERS: SkillTier[] = ['core', 'domain', 'none']

test('每种判定都有中文标签，且互不重复', () => {
  const labels = ALL_VERDICTS.map(verdictLabel)
  for (const l of labels) assert.ok(l && l.length > 0)
  assert.equal(new Set(labels).size, labels.length, '标签重复会导致两档看起来一样')
})

test('未知判定落到「未判定」而不是空串', () => {
  assert.equal(verdictLabel('whatever' as SkillVerdict), '未判定')
  assert.ok(verdictTone('whatever' as SkillVerdict).includes('red'))
})

test('判定颜色：可收录的偏绿，不采用的偏灰/黄', () => {
  assert.ok(verdictTone('adopted').includes('green'))
  assert.ok(verdictTone('mirrored').includes('sky'))
  assert.ok(verdictTone('deprecated').includes('gray'))
  for (const v of ALL_VERDICTS) assert.ok(verdictTone(v).length > 0)
})

test('三个层级都有标签与配色，且互不重复', () => {
  const labels = ALL_TIERS.map(tierLabel)
  assert.deepEqual(labels, ['通用', '领域', '不收'])
  assert.equal(new Set(labels).size, 3)
  for (const t of ALL_TIERS) assert.ok(tierTone(t).length > 0)
})

test('humanBytes 按量级换单位', () => {
  assert.equal(humanBytes(0), '0 B')
  assert.equal(humanBytes(-1), '0 B')
  assert.equal(humanBytes(512), '512 B')
  assert.equal(humanBytes(2048), '2 KB')
  assert.equal(humanBytes(2 * 1024 * 1024), '2.0 MB')
})

const pack = (name: string, verdict: SkillVerdict, tier: SkillTier = 'core'): SkillPack => ({
  name,
  path: `/x/${name}`,
  bytes: 1,
  files: 1,
  verdict,
  reason: 'r',
  tier,
  retain: tier !== 'none',
  category: 'c',
})

// 这条是这次重判的核心：领域包必须排在通用包之后，但**必须还在列表里**。
// 排到后面 = 不预设；仍在列表里 = 用得上。缺任一半都是错的。
test('sortPacks：通用在前、领域居中、不收录在最后', () => {
  const sorted = sortPacks([
    pack('none-x', 'deprecated', 'none'),
    pack('domain-a', 'project', 'domain'),
    pack('core-z', 'mirrored', 'core'),
    pack('core-a', 'adopted', 'core'),
  ])
  assert.deepEqual(
    sorted.map((p) => p.name),
    ['core-a', 'core-z', 'domain-a', 'none-x'],
  )
})

test('sortPacks：同层级内按「进了引擎的程度」排', () => {
  const sorted = sortPacks([
    pack('zeta', 'project', 'domain'),
    pack('beta', 'partial', 'domain'),
    pack('alpha', 'adopted', 'domain'),
  ])
  assert.deepEqual(
    sorted.map((p) => p.name),
    ['alpha', 'beta', 'zeta'],
  )
})

test('sortPacks 不改原数组', () => {
  const src = [pack('b', 'project'), pack('a', 'adopted')]
  sortPacks(src)
  assert.deepEqual(src.map((p) => p.name), ['b', 'a'])
})
