package handler

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"flowedge/backend/service"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WSHandler WebSocket 处理器
type WSHandler struct {
	hub *service.WSHub
}

func NewWSHandler(hub *service.WSHub) *WSHandler {
	return &WSHandler{hub: hub}
}

// HandleWS 处理 WebSocket 连接
func (h *WSHandler) HandleWS(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// 订阅事件
	eventChan := h.hub.Subscribe()
	defer h.hub.Unsubscribe(eventChan)

	// 发送欢迎消息
	conn.WriteJSON(map[string]interface{}{
		"type":    "connected",
		"message": "FlowEdge WebSocket connected",
	})

	// 监听事件并推送
	for event := range eventChan {
		err := conn.WriteJSON(event)
		if err != nil {
			log.Printf("websocket write failed: %v", err)
			break
		}
	}
}
