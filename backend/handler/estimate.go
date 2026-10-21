package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 提交前估算
//
// 为什么要有这个接口（2026-10-21）：
//
//	用户提交一条 8 镜 30 秒的片子前，唯一能知道"会不会跑 40 分钟 / 会不会 OOM"
//	的办法是翻 AGENTS.md 里手抄的经验区间。估算是把这个经验变成接口。
//
// 口径（必须写清，否则会被当成承诺）：
//   - 显存峰值取各节点 **max**（同一张卡串行，峰值不叠加）
//   - 时间取各节点 **sum**（串行执行；并行分支会高估，这是偏保守的方向）
//   - 每个数字都带区间与 confidence；没有样本时给 none 而不是编一个数
type EstimateHandler struct {
	db       *gorm.DB
	profiles *service.ProfileStore
	totalMB  int
}

func NewEstimateHandler(db *gorm.DB, profiles *service.ProfileStore, totalMB int) *EstimateHandler {
	return &EstimateHandler{db: db, profiles: profiles, totalMB: totalMB}
}

type estimateNode struct {
	Type string                 `json:"type"`
	ID   string                 `json:"id"`
	Data map[string]interface{} `json:"data"`
}

type estimateRequest struct {
	WorkflowID uint                     `json:"workflow_id"`
	Nodes      []map[string]interface{} `json:"nodes"`
	TotalMB    int                      `json:"total_mb"`
}

// Estimate POST /api/estimate
func (h *EstimateHandler) Estimate(c *gin.Context) {
	var req estimateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return
	}
	nodes := req.Nodes
	if len(nodes) == 0 && req.WorkflowID > 0 {
		if h.db == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "数据库不可用，请直接传 nodes"})
			return
		}
		var wf model.Workflow
		if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "工作流不存在"})
			return
		}
		if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "工作流节点数据损坏：" + err.Error()})
			return
		}
	}
	if len(nodes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请给 nodes 或 workflow_id"})
		return
	}

	total := req.TotalMB
	if total <= 0 {
		total = h.totalMB
	}
	if total <= 0 {
		total = 24576
	}

	items := make([]service.Prediction, 0, len(nodes))
	notes := []string{}
	var wallMin, wallMax int64
	peakMB, peakMaxMB := 0, 0
	worstConf := "high"

	// 口径：peak_mb 是期望值，peak_max_mb 是上界。
	// 「能不能跑」按上界判（保守），展示给人看用期望值。
	accumulate := func(pred service.Prediction) {
		if pred.PeakMB > peakMB {
			peakMB = pred.PeakMB
		}
		if pred.PeakMaxMB > peakMaxMB {
			peakMaxMB = pred.PeakMaxMB
		}
		worstConf = worseConfidence(worstConf, pred.Confidence)
	}

	for _, raw := range nodes {
		n := normalizeNode(raw)
		switch n.Type {
		case "shotlist":
			pred, count, note := h.estimateShotlist(n, total)
			items = append(items, pred)
			notes = append(notes, note)
			if pred.Confidence != "none" {
				wallMin += pred.WallMinMS * int64(count)
				wallMax += pred.WallMaxMS * int64(count)
			}
			accumulate(pred)
		case "text2image", "image2image", "inpaint", "upscale", "text2video":
			pred := h.estimateMedia(n, total)
			items = append(items, pred)
			if pred.Confidence != "none" {
				wallMin += pred.WallMinMS
				wallMax += pred.WallMaxMS
			}
			accumulate(pred)
		}
	}

	resp := gin.H{
		"items":         items,
		"peak_mb":       peakMB,
		"peak_max_mb":   peakMaxMB,
		"wall_min_ms":   wallMin,
		"wall_max_ms":   wallMax,
		"total_vram_mb": total,
		"fits":          peakMaxMB == 0 || peakMaxMB <= int(float64(total)*0.96),
		"confidence":    worstConf,
		"notes":         notes,
	}
	if len(items) == 0 {
		resp["notes"] = append(notes, "没有可估算的媒体节点（估算只覆盖出图/出视频/分镜出片）")
	}
	if peakMaxMB > int(float64(total)*0.96) {
		resp["warning"] = fmt.Sprintf("预计峰值上界 %d MB 超过可用 %d MB：大概率 OOM，"+
			"建议先降一档分辨率或用更少镜数试一条", peakMaxMB, total)
	}
	c.JSON(http.StatusOK, resp)
}

// ListProfiles GET /api/vram/profiles
func (h *EstimateHandler) ListProfiles(c *gin.Context) {
	if h.profiles == nil {
		c.JSON(http.StatusOK, gin.H{"profiles": []service.VRAMProfile{}, "note": "显存档案未启用"})
		return
	}
	items := h.profiles.All()
	c.JSON(http.StatusOK, gin.H{
		"profiles": items,
		"count":    len(items),
		"total_mb": h.totalMB,
	})
}

// normalizeNode 兼容两种输入：React Flow 节点（参数在 data 里）与扁平节点
func normalizeNode(raw map[string]interface{}) estimateNode {
	n := estimateNode{}
	if t, ok := raw["type"].(string); ok {
		n.Type = t
	}
	if id, ok := raw["id"].(string); ok {
		n.ID = id
	}
	n.Data = map[string]interface{}{}
	if d, ok := raw["data"].(map[string]interface{}); ok {
		for k, v := range d {
			n.Data[k] = v
		}
	}
	// 扁平写法（{"type":"text2image","width":1024}）也认，方便脚本调用
	for k, v := range raw {
		if k == "type" || k == "id" || k == "data" {
			continue
		}
		if _, exists := n.Data[k]; !exists {
			n.Data[k] = v
		}
	}
	return n
}

func (h *EstimateHandler) estimateMedia(n estimateNode, total int) service.Prediction {
	kind := n.Type
	if kind == "text2image" {
		kind = "image"
	} else if kind == "text2video" {
		kind = "video"
	}
	modelName := strFrom(n.Data, "model")
	steps := intFrom(n.Data, "steps")
	width, height := intFrom(n.Data, "width"), intFrom(n.Data, "height")
	if width <= 0 {
		width, height = 1024, 1024
	}
	frames := intFrom(n.Data, "frames")
	if h.profiles == nil {
		return service.Prediction{Kind: kind, Model: modelName, Confidence: "none",
			Basis: "显存档案未启用（引擎未注入 ProfileStore）"}
	}
	return h.profiles.Predict(kind, modelName, width, height, frames, steps)
}

// estimateShotlist 分镜出片：单镜估算 × 镜数（串行）
func (h *EstimateHandler) estimateShotlist(n estimateNode, total int) (service.Prediction, int, string) {
	count := 1
	if shots, ok := n.Data["shots"].([]interface{}); ok && len(shots) > 0 {
		count = len(shots)
	} else if plan, ok := n.Data["plan"].(string); ok && strings.TrimSpace(plan) != "" {
		if p, err := service.ParseShotPlan(plan); err == nil {
			count = len(p.Shots)
		}
	}
	width, height := intFrom(n.Data, "width"), intFrom(n.Data, "height")
	if width <= 0 {
		width, height = 608, 352
	}
	frames := intFrom(n.Data, "frames")
	if frames <= 0 {
		frames = 124
	}
	pred := service.Prediction{Kind: "h3", Confidence: "none", Basis: "显存档案未启用"}
	if h.profiles != nil {
		pred = h.profiles.Predict("h3", "", width, height, frames, intFrom(n.Data, "steps"))
	}
	note := fmt.Sprintf("分镜出片按 %d 镜串行累加（每镜 %dx%d / %d 帧）；单镜失败会整片失败，实际风险高于数字", count, width, height, frames)
	return pred, count, note
}

func worseConfidence(a, b string) string {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2, "none": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func intFrom(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	if v, ok := m[key].(int); ok {
		return v
	}
	if v, ok := m[key].(string); ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return int(f)
		}
	}
	return 0
}

func strFrom(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
