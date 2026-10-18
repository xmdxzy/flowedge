package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// RunHandler 运行维度的查询接口：全局列表、事件回放、trace。
// 此前只能按 workflow 查运行记录且硬编码 Limit(20)，也没有事件回放入口。
type RunHandler struct {
	db  *gorm.DB
	log *service.EventLogger
}

func NewRunHandler(db *gorm.DB, eventLog *service.EventLogger) *RunHandler {
	return &RunHandler{db: db, log: eventLog}
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

// ListAll 全局运行列表：?limit=50&offset=0&status=failed（默认 50，上限 200）
func (h *RunHandler) ListAll(c *gin.Context) {
	limit := parseIntDefault(c.Query("limit"), 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := parseIntDefault(c.Query("offset"), 0)
	status := c.Query("status")

	q := h.db.Model(&model.WorkflowRun{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	q.Count(&total)

	var runs []model.WorkflowRun
	q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&runs)

	c.JSON(http.StatusOK, gin.H{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"status": status,
		"runs":   runs,
	})
}

// Events 回放某次运行的事件流（含 token 汇总）
func (h *RunHandler) Events(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	if h.log == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "event log not enabled"})
		return
	}

	events, err := h.log.ListByRun(uint(id), 500)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"run_id": id,
		"count":  len(events),
		"usage":  service.UsageOfEvents(events),
		"events": events,
	})
}

// TraceSpan 单节点耗时片段（由 node_states 终态生成）
type TraceSpan struct {
	NodeID     string `json:"node_id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	ErrorCode  string `json:"error_code,omitempty"`
}

// Trace 运行级 trace：节点耗时 + token 汇总
func (h *RunHandler) Trace(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}

	var run model.WorkflowRun
	if err := h.db.First(&run, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}

	states := []model.NodeInstance{}
	if len(run.NodeStates) > 0 {
		if err := json.Unmarshal(run.NodeStates, &states); err != nil {
			states = []model.NodeInstance{}
		}
	}

	spans := make([]TraceSpan, 0, len(states))
	var total int64
	for _, n := range states {
		span := TraceSpan{NodeID: n.ID, Type: n.Type, Status: n.Status, ErrorCode: n.ErrorCode}
		if n.StartedAt != nil && n.EndedAt != nil {
			d := n.EndedAt.Sub(*n.StartedAt).Milliseconds()
			span.DurationMs = d
			total += d
		}
		spans = append(spans, span)
	}

	usage := service.TokenUsage{}
	if h.log != nil {
		if events, err := h.log.ListByRun(uint(id), 500); err == nil {
			usage = service.UsageOfEvents(events)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"run_id":     id,
		"status":     run.Status,
		"total_ms":   total,
		"usage":      usage,
		"spans":      spans,
		"started_at": run.StartedAt,
		"ended_at":   run.EndedAt,
	})
}
