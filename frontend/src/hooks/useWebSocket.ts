import { useEffect, useRef, useState, useCallback } from 'react'

export interface WSEvent {
  type: string
  node_id?: string
  data?: any
  message?: string
}

export function useWebSocket(onEvent: (event: WSEvent) => void) {
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const onEventRef = useRef(onEvent)
  onEventRef.current = onEvent
  // 重连退避：1s → 2s → 4s … 封顶 15s；卸载后不再重连
  const retryRef = useRef(0)
  const timerRef = useRef<number | null>(null)
  const disposedRef = useRef(false)

  useEffect(() => {
    disposedRef.current = false
    // 构建 WebSocket URL
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const wsUrl = `${protocol}//${window.location.host}/api/ws`
    let ws: WebSocket | null = null

    const connect = () => {
      ws = new WebSocket(wsUrl)
      wsRef.current = ws

      ws.onopen = () => {
        retryRef.current = 0
        setConnected(true)
        console.log('[WS] connected')
      }

      ws.onmessage = (event) => {
        try {
          const data: WSEvent = JSON.parse(event.data)
          onEventRef.current(data)
        } catch (err) {
          console.error('[WS] parse error', err)
        }
      }

      ws.onerror = (err) => {
        console.error('[WS] error', err)
      }

      ws.onclose = () => {
        setConnected(false)
        if (disposedRef.current) return
        // 这里绝不能 window.location.reload()：半小时的分镜任务中途只要抖一次，
        // reload 就会把未保存的画布和运行视图一起清掉（实测过的最致命的 UX 缺陷）。
        // 运行状态由 pollRun 独立收敛，重连即可，不需要刷新页面。
        const delay = Math.min(15000, 1000 * 2 ** retryRef.current)
        retryRef.current += 1
        console.log(`[WS] disconnected, retry in ${delay}ms`)
        timerRef.current = window.setTimeout(connect, delay)
      }
    }

    connect()

    return () => {
      disposedRef.current = true
      if (timerRef.current) window.clearTimeout(timerRef.current)
      ws?.close()
    }
  }, [])

  const sendMessage = useCallback((data: any) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data))
    }
  }, [])

  return { connected, sendMessage }
}
