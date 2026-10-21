import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { existsSync } from 'node:fs'
import path from 'node:path'

import { hintFor, isRetryable } from './errorHints'

// 错误码 → 人话的覆盖度测试。
//
// 背景（errorHints.ts 顶部注释）：后端定义了结构化错误码，前端一度把裸码丢给用户。
// 只要后端加一个新码而这里没翻译，用户又会看到机器词。所以按 Go 源码里的
// ErrorCode* 常量逐个对账，而不是手抄一份码表。

function findRepoRoot(): string {
  let dir = process.cwd()
  for (let i = 0; i < 6; i++) {
    if (existsSync(path.join(dir, 'backend', 'model', 'workflow.go'))) return dir
    dir = path.dirname(dir)
  }
  throw new Error(`找不到仓库根（cwd=${process.cwd()}）`)
}

function backendErrorCodes(): string[] {
  const src = readFileSync(path.join(findRepoRoot(), 'backend', 'model', 'workflow.go'), 'utf8')
  const re = /ErrorCode\w+\s*=\s*"([a-z_]+)"/g
  const out: string[] = []
  let m: RegExpExecArray | null
  while ((m = re.exec(src)) !== null) out.push(m[1])
  return out
}

test('后端每个错误码都有中文提示', () => {
  const codes = backendErrorCodes()
  assert.ok(codes.length >= 6, `只解析到 ${codes.length} 个错误码，解析规则可能过期`)
  const fallback = hintFor('__no_such_code__')
  for (const code of codes) {
    const hint = hintFor(code)
    assert.ok(hint.title && hint.title.length > 0, `${code} 没有标题`)
    assert.notEqual(hint, fallback, `${code} 落到了兜底文案，等于没翻译`)
  }
})

test('未知错误码走兜底，但不崩', () => {
  const hint = hintFor('something_new')
  assert.ok(hint.title.length > 0)
  assert.ok(hint.detail.length > 0)
})

test('无参调用也能给出可读文案', () => {
  const hint = hintFor()
  assert.ok(hint.title.length > 0)
})

test('只有可重试的码才返回 true', () => {
  assert.equal(isRetryable('timeout'), true)
  assert.equal(isRetryable('config_missing'), false, '缺依赖重试没有意义，应该去修环境')
  assert.equal(isRetryable('cycle'), false)
  assert.equal(isRetryable(undefined), false)
})

test('危险级别与修复动作要自洽：warn 类通常可重试', () => {
  const codes = backendErrorCodes()
  for (const code of codes) {
    const hint = hintFor(code)
    if (hint.level === 'warn' && hint.action === 'retry-node') {
      assert.equal(isRetryable(code), true, `${code} 提供了重试按钮，但 isRetryable 说不能重试`)
    }
  }
})
