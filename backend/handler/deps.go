package handler

import (
	"net/http"
	"strings"
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

// Reload 重新读取 data/deps.json 并重建启动规格（不重启引擎、不动已跑的进程）
//
// 返回 changes：字段级差异，空对象表示「配置没变」。
// endpoint 变更会标注「需重启引擎」，因为它不在本结构可控范围内。
func (h *DepHandler) Reload(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	changes := h.sup.Reload()
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"changes": changes,
		"count":   len(changes),
		"items":   h.sup.Status(),
	})
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

// Unload 卸载模型、保留进程（省显存又不付重启代价）。
//
// 为什么需要它：平台原来只有 start / stop 两个动作，而「把 H3 占着的 20GB 要回来」
// 只有 stop 一条路——下次冷启动 30–60 秒 + 权重重载 3 分钟。
// ComfyUI 的 /free 只卸权重、进程还在，是个缺了很久的中间档。
func (h *DepHandler) Unload(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	name := c.Param("name")
	res, err := h.sup.Unload(name)
	if err != nil {
		code := http.StatusBadGateway
		switch {
		case strings.Contains(err.Error(), "未知依赖"):
			code = http.StatusNotFound
		case strings.Contains(err.Error(), "未运行"):
			code = http.StatusConflict
		}
		c.JSON(code, gin.H{"error": err.Error(), "result": res})
		return
	}
	c.JSON(http.StatusOK, res)
}

// Reclaim 回收显存：清队列 → /free（或 /unload）→ **测量**是否真的释放了。
//
// 与 Unload 的分工：Unload 只发卸载请求；Reclaim 会先清掉 ComfyUI 队列里
// 卡住的 running/pending 项——实测任务超时后 ComfyUI 侧还在 VAE decode 里跑，
// 这时 /free 完全无效（还在跑就不会卸载），必须先把队列清掉。
func (h *DepHandler) Reclaim(c *gin.Context) {
	if h.sup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "依赖管理器未初始化"})
		return
	}
	name := c.Param("name")
	res, err := h.sup.ReclaimVRAM(name)
	if err != nil {
		code := http.StatusBadGateway
		if strings.Contains(err.Error(), "未知依赖") {
			code = http.StatusNotFound
		}
		c.JSON(code, gin.H{"error": err.Error(), "result": res})
		return
	}
	c.JSON(http.StatusOK, res)
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
