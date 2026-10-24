import type { WsEvent } from '../api/types'

// 实时事件的纯逻辑部分（不碰 WebSocket，方便单测）：
//   - 什么事件该触发刷新
//   - 断线重连的指数退避
//   - 300ms 去抖器（5 个 node_progress 只打一次接口）
//
// 后端实际只发 node_started / node_progress / node_retry / node_failed / node_finished，
// 外加 loop_iteration / quality_round / shot_progress——**没有 `run_*` 事件**。
// 所以判定用的是「这条事件带不带当前聚焦的 run_id」，不按事件名白名单（白名单会漏事件）。

/** 事件是否属于某条运行。runId 为空（没聚焦）时一律 false。 */
export function belongsToRun(
  event: Pick<WsEvent, 'type' | 'run_id'> | null | undefined,
  runId: number | null,
): boolean {
  if (!event || runId == null) return false
  return typeof event.run_id === 'number' && event.run_id === runId
}

/** 指数退避：1s → 2s → 4s … 封顶 30s（attempt 从 0 起） */
export function nextReconnectDelay(attempt: number, baseMs = 1000, maxMs = 30000): number {
  const n = Number.isFinite(attempt) ? Math.max(0, Math.floor(attempt)) : 0
  // 先截断指数：2 ** 1024 是 Infinity，乘底数后 Math.min 虽然还能兜住，但没必要算它
  const exp = Math.min(n, 30)
  return Math.min(maxMs, baseMs * 2 ** exp)
}

export interface TimerApi {
  setTimeout(cb: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
}

const defaultTimers: TimerApi = {
  setTimeout: (cb, ms) => setTimeout(cb, ms),
  clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
}

export interface Debouncer {
  /** 重新计时：连续调用只会在最后一次之后 delayMs 跑一次 run */
  trigger(): void
  /** 立即执行挂起的那一次（没有挂起则什么都不做） */
  flush(): void
  cancel(): void
  readonly pending: boolean
}

/**
 * 去抖器。
 * `timers` 可注入：单测里传假时钟就能断言「N 次 trigger = 1 次 run」而不用 sleep。
 */
export function createDebouncer(
  delayMs: number,
  run: () => void,
  timers: TimerApi = defaultTimers,
): Debouncer {
  let handle: unknown = null
  let pending = false

  const fire = () => {
    handle = null
    pending = false
    run()
  }

  return {
    trigger() {
      if (handle !== null) timers.clearTimeout(handle)
      pending = true
      handle = timers.setTimeout(fire, delayMs)
    },
    flush() {
      if (handle === null) return
      timers.clearTimeout(handle)
      fire()
    },
    cancel() {
      if (handle !== null) timers.clearTimeout(handle)
      handle = null
      pending = false
    },
    get pending() {
      return pending
    },
  }
}
