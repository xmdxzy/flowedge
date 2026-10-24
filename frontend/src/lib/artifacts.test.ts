import { test } from 'node:test'
import assert from 'node:assert/strict'

import type { ArtifactItem } from '../api/types'
import { countByKind, filterArtifacts, joinArtifactPath, kindOf } from './artifacts'

// 产物归类必须与 backend/handler/artifact.go 的 artifactExt 一致：
// 后端列出来的东西，前端要能摆进对应分组；后端没列的扩展名不能凭空认领。

const items: ArtifactItem[] = [
  { name: 'a.png', kind: 'image', size: 1, mod_time: 3 },
  { name: 'b.JPG', kind: 'image', size: 1, mod_time: 2 },
  { name: 'c.mp4', kind: 'video', size: 1, mod_time: 1 },
  { name: 'd.wav', kind: 'audio', size: 1, mod_time: 0 },
]

test('按扩展名归类：大小写不敏感', () => {
  assert.equal(kindOf('a.png'), 'image')
  assert.equal(kindOf('b.JPG'), 'image')
  assert.equal(kindOf('c.jpeg'), 'image')
  assert.equal(kindOf('d.webp'), 'image')
  assert.equal(kindOf('e.mp4'), 'video')
  assert.equal(kindOf('f.webm'), 'video')
  assert.equal(kindOf('g.wav'), 'audio')
  assert.equal(kindOf('h.mp3'), 'audio')
})

test('不认识的扩展名返回 null，不猜成 image', () => {
  assert.equal(kindOf('notes.txt'), null)
  assert.equal(kindOf('noext'), null)
  assert.equal(kindOf('.gitignore'), null)
})

test('后端已给 kind 时以后端为准；给了不认识的 kind 才退回扩展名', () => {
  assert.equal(kindOf('weird.bin', 'video'), 'video')
  assert.equal(kindOf('weird.bin', 'nonsense'), null)
  assert.equal(kindOf('real.png', 'nonsense'), 'image')
})

test('过滤：all 原样返回，其余只留对应类别', () => {
  assert.equal(filterArtifacts(items, 'all').length, 4)
  assert.deepEqual(
    filterArtifacts(items, 'image').map((i) => i.name),
    ['a.png', 'b.JPG'],
  )
  assert.deepEqual(
    filterArtifacts(items, 'video').map((i) => i.name),
    ['c.mp4'],
  )
  assert.deepEqual(
    filterArtifacts(items, 'audio').map((i) => i.name),
    ['d.wav'],
  )
})

test('计数包含 all 与三类', () => {
  assert.deepEqual(countByKind(items), { all: 4, image: 2, video: 1, audio: 1 })
  assert.deepEqual(countByKind([]), { all: 0, image: 0, video: 0, audio: 0 })
})

test('复制路径：按目录分隔符拼接，不在盘符路径里混入正斜杠', () => {
  assert.equal(joinArtifactPath('C:\\flowedge\\data\\generated', 'a.png'), 'C:\\flowedge\\data\\generated\\a.png')
  assert.equal(joinArtifactPath('/srv/flowedge/generated', 'a.png'), '/srv/flowedge/generated/a.png')
  assert.equal(joinArtifactPath('/srv/flowedge/generated/', 'a.png'), '/srv/flowedge/generated/a.png')
  assert.equal(joinArtifactPath('', 'a.png'), 'a.png')
})
