import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  elapsedMs,
  formatBytes,
  formatDurationMs,
  formatEstSeconds,
  formatTime,
} from './format'

// est_seconds 的展示边界：30s / 90s / 1800s 是产品上真会遇到的三档
// （image=60、video=240、video_from_image=420、长片 1800+）。
test('est_seconds → 「约 N 秒 / 约 N 分钟 / 约 N 小时」', () => {
  assert.equal(formatEstSeconds(30), '约 30 秒')
  assert.equal(formatEstSeconds(60), '约 1 分钟')
  assert.equal(formatEstSeconds(90), '约 1.5 分钟')
  assert.equal(formatEstSeconds(240), '约 4 分钟')
  assert.equal(formatEstSeconds(1800), '约 30 分钟')
  assert.equal(formatEstSeconds(3600), '约 1 小时')
  assert.equal(formatEstSeconds(5400), '约 1.5 小时')
})

test('没有估时时说「时长未知」，不说「约 0 秒」', () => {
  assert.equal(formatEstSeconds(0), '时长未知')
  assert.equal(formatEstSeconds(undefined), '时长未知')
  assert.equal(formatEstSeconds(null), '时长未知')
  assert.equal(formatEstSeconds(Number.NaN), '时长未知')
})

test('耗时格式化：秒 / 分 / 时三档，非法值给破折号', () => {
  assert.equal(formatDurationMs(0), '0 秒')
  assert.equal(formatDurationMs(12_000), '12 秒')
  assert.equal(formatDurationMs(65_000), '1 分 05 秒')
  assert.equal(formatDurationMs(60_000), '1 分')
  assert.equal(formatDurationMs(3_780_000), '1 时 03 分')
  assert.equal(formatDurationMs(3_600_000), '1 时')
  assert.equal(formatDurationMs(null), '—')
  assert.equal(formatDurationMs(-1), '—')
})

test('elapsedMs：running 用 now 兜底，缺 started_at 返回 null', () => {
  const start = '2026-10-25T03:00:00Z'
  assert.equal(elapsedMs(start, '2026-10-25T03:00:05Z', 0), 5000)
  assert.equal(elapsedMs(start, null, new Date('2026-10-25T03:00:07Z').getTime()), 7000)
  assert.equal(elapsedMs(null, null, 1), null)
  assert.equal(elapsedMs('not-a-date', null, 1), null)
})

test('字节格式化', () => {
  assert.equal(formatBytes(512), '512 B')
  assert.equal(formatBytes(2048), '2 KB')
  assert.equal(formatBytes(1.5 * 1024 * 1024), '1.5 MB')
  assert.equal(formatBytes(null), '—')
})

test('时间格式化：坏值不抛，给破折号', () => {
  assert.equal(formatTime(null), '—')
  assert.equal(formatTime('nope'), '—')
  assert.match(formatTime('2026-10-25T03:04:05Z'), /^\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/)
})
