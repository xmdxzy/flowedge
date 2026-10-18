package service

import (
	"log"
	"sync"
)

// Event 执行事件
type Event struct {
	Type    string                 `json:"type"` // node_started, node_finished, node_failed, workflow_finished
	NodeID  string                 `json:"node_id,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Message string                 `json:"message,omitempty"`
}

// WSHub 简单的事件广播中心
type WSHub struct {
	subscribers []chan Event
	mu          sync.RWMutex
}

func NewWSHub() *WSHub {
	return &WSHub{}
}

// Subscribe 订阅事件
func (h *WSHub) Subscribe() chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan Event, 100)
	h.subscribers = append(h.subscribers, ch)
	return ch
}

// Unsubscribe 取消订阅
func (h *WSHub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i, sub := range h.subscribers {
		if sub == ch {
			h.subscribers = append(h.subscribers[:i], h.subscribers[i+1:]...)
			close(ch)
			break
		}
	}
}

// Broadcast 广播事件给所有订阅者
func (h *WSHub) Broadcast(event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if event.Type != "node_token" { // token 事件高频，不刷日志
		log.Printf("[Event] %s: %s", event.Type, event.NodeID)
	}

	for _, ch := range h.subscribers {
		select {
		case ch <- event:
		default:
			// 缓冲区满了，跳过这个订阅者
		}
	}
}
