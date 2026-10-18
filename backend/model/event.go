package model

import (
	"encoding/json"
	"time"
)

// RunEvent 执行事件（持久化）。
//
// 此前事件只经 WebSocket 广播，订阅者缓冲区满即丢弃，进程重启后完全不可追溯。
// 落库后运行可回放、可审计，也是 trace 视图的数据源。
// 高频的 node_token 不入库（见 service/event_log.go）。
type RunEvent struct {
	ID        uint            `gorm:"primaryKey" json:"id"`
	RunID     uint            `gorm:"index" json:"run_id"`
	Seq       int             `json:"seq"` // 同一 run 内的递增序号，保证回放顺序
	Type      string          `gorm:"size:32;index" json:"type"`
	NodeID    string          `gorm:"size:64" json:"node_id,omitempty"`
	Message   string          `gorm:"type:text" json:"message,omitempty"`
	Data      json.RawMessage `gorm:"type:text" json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}
