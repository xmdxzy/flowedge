package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// HealthHandler 健康检查与环境自检
type HealthHandler struct {
	gpu        *service.GPUScheduler
	baseDir    string
	ollamaBase string
	comfyBase  string
	asrBase    string
	ttsBase    string
	version    string
	startedAt  time.Time
}

func NewHealthHandler(gpu *service.GPUScheduler, baseDir, ollamaBase, comfyBase, asrBase, ttsBase, version string) *HealthHandler {
	return &HealthHandler{
		gpu:        gpu,
		baseDir:    baseDir,
		ollamaBase: ollamaBase,
		comfyBase:  comfyBase,
		asrBase:    asrBase,
		ttsBase:    ttsBase,
		version:    version,
		startedAt:  time.Now(),
	}
}

// Health 轻量探活（给启动器 / 容器 healthcheck 用）
func (h *HealthHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Capabilities 部署环境能力矩阵：缺什么、怎么补，一目了然
func (h *HealthHandler) Capabilities(c *gin.Context) {
	caps := service.ProbeCapabilities(h.gpu, h.baseDir, h.ollamaBase, h.comfyBase, h.asrBase, h.ttsBase)
	c.JSON(http.StatusOK, gin.H{
		"capabilities":   caps,
		"version":        h.version,
		"uptime_seconds": int(time.Since(h.startedAt).Seconds()),
	})
}
