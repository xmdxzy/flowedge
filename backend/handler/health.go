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
	engine     NodeTypeLister
	baseDir    string
	ollamaBase string
	comfyBase  string
	h3Base     string
	asrBase    string
	ttsBase    string
	version    string
	startedAt  time.Time
}

// NodeTypeLister 引擎暴露已注册节点类型（避免 handler 直接依赖具体引擎实现）
type NodeTypeLister interface {
	NodeTypes() []string
}

func NewHealthHandler(gpu *service.GPUScheduler, baseDir, ollamaBase, comfyBase, h3Base, asrBase, ttsBase, version string) *HealthHandler {
	return &HealthHandler{
		gpu:        gpu,
		baseDir:    baseDir,
		ollamaBase: ollamaBase,
		comfyBase:  comfyBase,
		h3Base:     h3Base,
		asrBase:    asrBase,
		ttsBase:    ttsBase,
		version:    version,
		startedAt:  time.Now(),
	}
}

// SetNodeTypeLister 注入引擎（用于 /api/capabilities 下发节点类型清单）
func (h *HealthHandler) SetNodeTypeLister(l NodeTypeLister) { h.engine = l }

// Health 轻量探活（给启动器 / 容器 healthcheck 用）
func (h *HealthHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Capabilities 部署环境能力矩阵：缺什么、怎么补，一目了然
func (h *HealthHandler) Capabilities(c *gin.Context) {
	caps := service.ProbeCapabilities(h.gpu, h.baseDir, h.ollamaBase, h.comfyBase, h.h3Base, h.asrBase, h.ttsBase)
	payload := gin.H{
		"capabilities":   caps,
		"version":        h.version,
		"uptime_seconds": int(time.Since(h.startedAt).Seconds()),
	}
	// 节点类型清单下发：新增节点后前端不再需要手动同步两份代码
	if h.engine != nil {
		payload["node_types"] = h.engine.NodeTypes()
	}
	c.JSON(http.StatusOK, payload)
}
