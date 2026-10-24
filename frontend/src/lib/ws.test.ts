import { test } from 'node:test'
import assert from 'node:assert/strict'

import { belongsToRun, createDebouncer, nextReconnectDelay, type TimerApi } from './ws'

// WS 的纯逻辑部分：去抖、事件归属、重连退避。
// 不连真 WebSocket——那属于浏览器，这里只钉住「算法对不对」。

function fakeTimers() {
  let seq = 0
  const pending = new Map<number, { cb: () => void; ms: number }>()
  const api: TimerApi = {
    setTimeout(cb, ms) {
      const id = ++seq
      pending.set(id, { cb, ms })
      return id
    },
    clearTimeout(handle) {
      pending.delete(handle as number)
    },
  }
  return {
    api,
    pendingCount: () => pending.size,
    lastDelay: () => [...pending.values()].at(-1)?.ms,
    tick() {
      const cbs = [...pending.values()].map((v) => v.cb)
      pending.clear()
      for (const cb of cbs) cb()
    },
  }
}

test('300ms 去抖：连续 5 个事件只打一次接口', () => {
  const timers = fakeTimers()
  let calls = 0
  const debounce = createDebouncer(300, () => {
    calls += 1
  }, timers.api)

  for (let i = 0; i < 5; i++) debounce.trigger()
  assert.equal(timers.pendingCount(), 1, '每次 trigger 都要清掉上一个定时器，否则会攒出 5 次请求')
  assert.equal(timers.lastDelay(), 300)
  assert.equal(calls, 0, '去抖窗口内不能立刻打接口')
  assert.equal(debounce.pending, true)

  timers.tick()
  assert.equal(calls, 1)
  assert.equal(debounce.pending, false)
})

test('flush 立即执行挂起的那次；没有挂起时是空操作', () => {
  const timers = fakeTimers()
  let calls = 0
  const debounce = createDebouncer(300, () => {
    calls += 1
  }, timers.api)

  debounce.flush()
  assert.equal(calls, 0, '没挂起时不应凭空执行')

  debounce.trigger()
  debounce.flush()
  assert.equal(calls, 1)
  assert.equal(timers.pendingCount(), 0, 'flush 之后不能残留定时器（否则会执行第二次）')

  debounce.flush()
  assert.equal(calls, 1)
})

test('cancel 丢弃挂起的那次', () => {
  const timers = fakeTimers()
  let calls = 0
  const debounce = createDebouncer(300, () => {
    calls += 1
  }, timers.api)

  debounce.trigger()
  debounce.cancel()
  assert.equal(debounce.pending, false)
  assert.equal(timers.pendingCount(), 0)
  timers.tick()
  assert.equal(calls, 0)
})

test('去抖窗口结束后还能再次触发（不是一次性）', () => {
  const timers = fakeTimers()
  let calls = 0
  const debounce = createDebouncer(300, () => {
    calls += 1
  }, timers.api)

  debounce.trigger()
  timers.tick()
  debounce.trigger()
  timers.tick()
  assert.equal(calls, 2)
})

test('指数退避：1s → 2s → 4s … 封顶 30s', () => {
  assert.equal(nextReconnectDelay(0), 1000)
  assert.equal(nextReconnectDelay(1), 2000)
  assert.equal(nextReconnectDelay(2), 4000)
  assert.equal(nextReconnectDelay(4), 16000)
  assert.equal(nextReconnectDelay(5), 30000)
  assert.equal(nextReconnectDelay(50), 30000)
  assert.equal(nextReconnectDelay(1000), 30000, '大数不能溢出成 Infinity/NaN')
})

test('退避参数对负数/小数/NaN 都是安全的', () => {
  assert.equal(nextReconnectDelay(-3), 1000)
  assert.equal(nextReconnectDelay(2.9), 4000)
  assert.equal(nextReconnectDelay(Number.NaN), 1000)
  assert.equal(nextReconnectDelay(0, 500, 5000), 500)
})

test('只有当前聚焦 run 的事件才算数', () => {
  assert.equal(belongsToRun({ type: 'node_progress', run_id: 7 }, 7), true)
  assert.equal(belongsToRun({ type: 'node_progress', run_id: 8 }, 7), false)
  assert.equal(belongsToRun({ type: 'connected' }, 7), false, '欢迎消息没有 run_id，不能触发刷新')
  assert.equal(belongsToRun({ type: 'node_progress', run_id: 7 }, null), false)
  assert.equal(belongsToRun(null, 7), false)
})
