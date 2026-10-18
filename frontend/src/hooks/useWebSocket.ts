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

  useEffect(() => {
    // 构建 WebSocket URL
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const wsUrl = `${protocol}//${window.location.host}/api/ws`

    const ws = new WebSocket(wsUrl)
    wsRef.current = ws

    ws.onopen = () => {
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

    ws.onclose = () => {
      setConnected(false)
      console.log('[WS] disconnected')
      // 3秒后重连
      setTimeout(() => {
        console.log('[WS] reconnecting...')
        window.location.reload()
      }, 3000)
    }

    ws.onerror = (err) => {
      console.error('[WS] error', err)
    }

    return () => {
      ws.close()
    }
  }, [])

  const sendMessage = useCallback((data: any) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data))
    }
  }, [])

  return { connected, sendMessage }
}
