package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// DepHandler 依赖进程管理（ComfyUI / ASR 一键启停）
type DepHandler struct {
	sup *service.Supervisor
}

func NewDepHandler(sup *service.Supervisor) *DepHandler {
	return &DepHandler{sup: sup}
}

// List 依赖状态列表
func (h *DepHandler) List(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusOK, gin.H{"items": []service.DepStatus{}, "count": 0})
		return
	}
	items := h.sup.Status()
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

// Start 异步启动（ComfyUI 冷启动可能要几十秒，不能卡住 HTTP 请求）
func (h *DepHandler) Start(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	name := c.Param("name")
	if _, ok := h.sup.Spec(name); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "未知依赖：" + name})
		return
	}
	go func() {
		if err := h.sup.Start(name); err != nil {
			// 错误通过 /api/deps 的 last_error 回传，避免这里无处可写
			_ = err
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"name": name, "status": "starting"})
}

// Stop 停止由本进程拉起的依赖
func (h *DepHandler) Stop(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	if err := h.sup.Stop(c.Param("name")); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "stopped"})
}

// Wait 等待就绪（供脚本/自动化使用，最多等 120 秒）
func (h *DepHandler) Wait(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	name := c.Param("name")
	if err := h.sup.WaitReady(name, 120*time.Second); err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error(), "ready": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ready": true})
}
