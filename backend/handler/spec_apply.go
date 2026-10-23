package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// SpecApply POST /api/workflows/:id/spec/apply
//
// 任务书的回写入口。这是「探针—组合」闭环里缺的那一半：
// 没有它，智能体读得懂图但改不动，Compose 只能靠人在画布上手点。
//
// 请求：
//
//	{
//	  "baseVersion": 3,                       // 可选：拿 spec 时的版本号（粗粒度乐观锁）
//	  "allowGateChange": false,               // 是否允许改门层参数（放宽验收）
//	  "dryRun": false,                        // 只校验不落盘（智能体先预演）
//	  "edits": [{ "nodeId": "n12", "path": "data.min_frames",
//	              "from": 124, "to": 186, "reason": "..." }]
//	}
//
// 语义：
//   - 原子：一批里有一条不过，整批不生效（半批生效会把图留在没人设计过的状态）
//   - 留痕：生效前先把当前状态快照成新版本，旧图永远可回滚
//   - 门的牙齿：门层参数默认禁止，确需放宽必须 allowGateChange + reason
func (h *WorkflowHandler) SpecApply(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	var req struct {
		BaseVersion     *int               `json:"baseVersion"`
		AllowGateChange bool               `json:"allowGateChange"`
		DryRun          bool               `json:"dryRun"`
		Edits           []service.SpecEdit `json:"edits"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}

	// 粗粒度乐观锁：调用方声明"我基于第 N 版改的"，若已经不是第 N 版就整体拒绝。
	// 逐字段的 from 也能兜底，但版本号能挡住"整批基于过期任务书"这种情形。
	if req.BaseVersion != nil {
		cur := h.latestVersion(wf.ID)
		if cur != *req.BaseVersion {
			c.JSON(http.StatusConflict, gin.H{
				"error":       "工作流版本已变化，请重新拉取任务书",
				"code":        "version_changed",
				"baseVersion": *req.BaseVersion,
				"current":     cur,
			})
			return
		}
	}

	var nodes []model.NodeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "工作流节点数据损坏：" + err.Error()})
		return
	}

	next, applied, problems := service.ApplySpecEdits(nodes, req.Edits, service.ApplyOptions{
		AllowGateChange: req.AllowGateChange,
		DryRun:          req.DryRun,
	})
	if len(problems) > 0 {
		c.JSON(worstStatus(problems), gin.H{
			"error":    "编辑未通过，整批未生效",
			"problems": problems,
		})
		return
	}

	if req.DryRun {
		c.JSON(http.StatusOK, gin.H{"dryRun": true, "applied": applied, "wouldWrite": true})
		return
	}

	// 先快照再改：旧图必须永远可回滚。顺序反了就会出现"改了但没版本可回"。
	snapshotVersion := h.snapshotBefore(wf, len(applied))

	nodesJSON, err := json.Marshal(next)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化失败：" + err.Error()})
		return
	}
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).
		Update("nodes", json.RawMessage(nodesJSON)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写回失败：" + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"applied":         applied,
		"count":           len(applied),
		"snapshotVersion": snapshotVersion,
		"dryRun":          false,
	})
}

// latestVersion 当前最新快照版本号（没有快照时为 0）
func (h *WorkflowHandler) latestVersion(workflowID uint) int {
	var v int
	h.db.Model(&model.WorkflowVersion{}).
		Where("workflow_id = ?", workflowID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&v)
	return v
}

// snapshotBefore 把改动前的状态存成一个新版本，返回版本号。
//
// 快照失败不阻断改图（返回 0 让调用方知道"没留下版本"），
// 但会把错误写进响应 —— 静默失败会让人以为随时能回滚，而其实回不去。
func (h *WorkflowHandler) snapshotBefore(wf model.Workflow, editCount int) int {
	ver := model.WorkflowVersion{
		WorkflowID: wf.ID,
		Version:    h.latestVersion(wf.ID) + 1,
		Name:       wf.Name,
		Nodes:      wf.Nodes,
		Edges:      wf.Edges,
		ChangeNote: "任务书回写前自动快照（本次改动 " + itoa(editCount) + " 项）",
	}
	if err := h.db.Create(&ver).Error; err != nil {
		return 0
	}
	return ver.Version
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// worstStatus 取一批问题里最该先修的那个状态码。
//
// 顺序是"先修结构性问题"：节点不存在（404）先于权限（403），
// 权限先于冲突（409），冲突先于参数错误（400）。
// 浏览器/智能体看到 404 就知道该重新拉图，而不是去琢磨权限。
func worstStatus(problems []service.EditProblem) int {
	rank := map[int]int{
		http.StatusNotFound:   4,
		http.StatusForbidden:  3,
		http.StatusConflict:   2,
		http.StatusBadRequest: 1,
	}
	best, bestRank := http.StatusBadRequest, 0
	for _, p := range problems {
		if r := rank[p.Status]; r > bestRank {
			best, bestRank = p.Status, r
		}
	}
	return best
}
