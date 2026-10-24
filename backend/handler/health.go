package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthHandler 探活与版本信息。
//
// 收敛后它只回答"引擎在不在、哪一版、跑了多久"：依赖状态在 /api/deps，
// 显存在 GPU 采样里，运行进度在 /api/runs —— 一件事只有一个出口。
type HealthHandler struct {
	version   string
	startedAt time.Time
}

func NewHealthHandler(version string) *HealthHandler {
	return &HealthHandler{version: version, startedAt: time.Now()}
}

// Health 轻量探活 + 版本/运行时长。
//
// 只给 {"status":"ok"} 时，看板和脚本都无法判断"这是哪一版、跑了多久"。
func (h *HealthHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"version":        h.version,
		"uptime_seconds": int(time.Since(h.startedAt).Seconds()),
	})
}
