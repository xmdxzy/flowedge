package handler

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// TriggerHandler 工作流触发器：Webhook（外部调用）+ Cron（定时）
type TriggerHandler struct {
	db *gorm.DB
	wf *WorkflowHandler
}

func NewTriggerHandler(db *gorm.DB, wf *WorkflowHandler) *TriggerHandler {
	return &TriggerHandler{db: db, wf: wf}
}

type triggerRequest struct {
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
}

func randomToken() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("tok%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// Get 读取触发器配置
func (h *TriggerHandler) Get(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	desc := ""
	if wf.CronExpr != "" {
		if d, err := service.ValidateCron(wf.CronExpr); err == nil {
			desc = d
		}
	}

	resp := gin.H{
		"workflow_id":       wf.ID,
		"token":             wf.TriggerToken,
		"cron":              wf.CronExpr,
		"enabled":           wf.TriggerEnabled,
		"cron_description":  desc,
		"last_triggered_at": wf.LastTriggeredAt,
	}
	if wf.TriggerToken != "" {
		resp["webhook_path"] = "/api/hooks/" + wf.TriggerToken
		resp["curl_example"] = fmt.Sprintf(
			"curl -X POST http://127.0.0.1:8080/api/hooks/%s", wf.TriggerToken)
	}
	c.JSON(http.StatusOK, resp)
}

// Update 保存触发器配置（首次启用自动生成 token）
func (h *TriggerHandler) Update(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	var req triggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Cron != "" {
		if _, err := service.ValidateCron(req.Cron); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cron 表达式无效：" + err.Error()})
			return
		}
	}

	updates := map[string]interface{}{
		"trigger_enabled": req.Enabled,
		"cron_expr":       req.Cron,
	}
	if wf.TriggerToken == "" {
		updates["trigger_token"] = randomToken()
	}
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.Get(c)
}

// Rotate 重新生成 token（旧链接立即失效）
func (h *TriggerHandler) Rotate(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).
		Update("trigger_token", randomToken()).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.Get(c)
}

// Webhook 公开触发端点：token 即凭据，无需额外鉴权
func (h *TriggerHandler) Webhook(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing token"})
		return
	}

	var wf model.Workflow
	if err := h.db.Where("trigger_token = ?", token).First(&wf).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invalid trigger token"})
		return
	}

	run := h.wf.StartRun(&wf)
	now := time.Now()
	h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Update("last_triggered_at", &now)

	c.JSON(http.StatusAccepted, gin.H{
		"run_id":      run.ID,
		"workflow_id": wf.ID,
		"workflow":    wf.Name,
		"status":      "running",
		"trigger":     "webhook",
	})
}
