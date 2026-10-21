import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render } from '@testing-library/react'

import ToastHost, { toast } from './Toast'
import { fire } from '../test-utils'

// Toast 是模块级订阅（不引 context），所以测试顺序有讲究：
// 必须先挂 ToastHost 再 toast()，否则消息发出去没人接——这也是真实使用顺序。

test('先挂宿主再 toast，消息能显示出来', () => {
  const { container } = render(<ToastHost />)
  // toast() 是模块级订阅 → 在 act 里发，setState 才会被刷新到 DOM
  fire(() => {
    toast('保存成功', 'success')
  })
  assert.ok(container.textContent?.includes('保存成功'), `实际内容：${container.textContent}`)
})

test('多条消息同时存在（不互相覆盖）', () => {
  const { container } = render(<ToastHost />)
  fire(() => {
    toast('第一条')
    toast('第二条', 'error')
  })
  const text = container.textContent ?? ''
  assert.ok(text.includes('第一条') && text.includes('第二条'), `实际内容：${text}`)
})

test('没挂宿主时 toast() 不抛（早于挂载调用是常见情况）', () => {
  assert.doesNotThrow(() => toast('孤零零的一条'))
})

test('toast() 返回自增 id，便于上层按 id 去重/关闭', () => {
  const a = toast('a')
  const b = toast('b')
  assert.ok(b > a)
})
