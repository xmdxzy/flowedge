import { useEffect, useRef, useState } from 'react'

import type { WsEvent } from '../api/types'
import { nextReconnectDelay } from '../lib/ws'

// 实时事件连接：断线自动重连（指数退避封顶 30s），并把「是否连着」交给界面标注。
//
// 为什么不用 window.location.reload()：一条出片可能跑几分钟，抖一下就把正在看的
// 运行详情清空，比不重连更糟。重连只重建连接，界面状态由 REST 刷新兜底。

export function wsURL(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${window.location.host}/api/ws`
}

export function useRealtime(onEvent: (event: WsEvent) => void): { connected: boolean } {
  const [connected, setConnected] = useState(false)
  // 回调每次渲染都是新函数，用 ref 存住，避免因为它变化而重建连接
  const handler = useRef(onEvent)
  handler.current = onEvent

  useEffect(() => {
    let disposed = false
    let socket: WebSocket | null = null
    let attempt = 0
    let timer: ReturnType<typeof setTimeout> | null = null

    const connect = () => {
      if (disposed) return
      let ws: WebSocket
      try {
        ws = new WebSocket(wsURL())
      } catch {
        scheduleReconnect()
        return
      }
      socket = ws

      ws.onopen = () => {
        attempt = 0
        setConnected(true)
      }
      ws.onmessage = (msg) => {
        try {
          handler.current(JSON.parse(String(msg.data)) as WsEvent)
        } catch {
          // 非 JSON 消息忽略：后端只发 JSON，解析失败说明协议漂移，不该炸界面
        }
      }
      ws.onerror = () => {
        // onclose 一定会跟着来，重连只在一处调度
      }
      ws.onclose = () => {
        setConnected(false)
        scheduleReconnect()
      }
    }

    const scheduleReconnect = () => {
      if (disposed) return
      const delay = nextReconnectDelay(attempt)
      attempt += 1
      timer = setTimeout(connect, delay)
    }

    connect()

    return () => {
      disposed = true
      if (timer) clearTimeout(timer)
      socket?.close()
    }
  }, [])

  return { connected }
}
