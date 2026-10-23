package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 晋升台账的两个端点：
//
//	POST /api/evolution/promote     人按下晋升或保留（唯一能写 action=promote/keep 的地方）
//	GET  /api/evolution/promotions  台账查询（谁在哪天凭什么判定了哪个版本）
//
// 与 decide 的分工：decide 给引擎结论（record=true 时归档为 archive），
// 这里记的是**人的判定**。两者都落同一张表，用 source 字段区分 ——
// 审计时既能看到"引擎当时怎么判"，也能看到"人最后按的是什么"。

// Promote POST /api/evolution/promote
//
// body: { workflowId, action:"promote"|"keep", fromVersion?, toVersion?,
//
//	reason, actor?, note?, apply? }
//
// action=promote 且 apply=true 时，把 toVersion 的图写成当前图（先快照，可回滚）——
// 写图路径与 /evolution/rollback 完全同构：没有快照就不写。
func (h *EvolutionHandler) Promote(c *gin.Context) {
	var req struct {
		WorkflowID  uint   `json:"workflowId"`
		Action      string `json:"action"`
		FromVersion int    `json:"fromVersion"`
		ToVersion   int    `json:"toVersion"`
		Reason      string `json:"reason"`
		Actor       string `json:"actor"`
		Note        string `json:"note"`
		Apply       bool   `json:"apply"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	// 端点只接受人按的两个键：rollback 走 /evolution/rollback（它会自己落库），
	// archive 是引擎结论专用，人不该手写。
	if req.Action != model.PromotionActionPromote && req.Action != model.PromotionActionKeep {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "action 必须是 promote 或 keep（回滚请用 POST /api/evolution/rollback，它会自动落库）",
		})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "reason 必填：没有理由的晋升/保留不许落库",
			"hint":  "写清「凭什么」：门分差多少、几对几、哪个指标动的。三个月后复核靠的就是这一句",
		})
		return
	}

	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	rec := model.Promotion{
		WorkflowID:  req.WorkflowID,
		Action:      req.Action,
		Source:      model.PromotionSourcePromote,
		FromVersion: req.FromVersion,
		ToVersion:   req.ToVersion,
		Reason:      req.Reason,
		Actor:       req.Actor,
		Note:        req.Note,
	}

	resp := gin.H{"action": req.Action, "workflowId": req.WorkflowID}

	if req.Action == model.PromotionActionPromote {
		if req.ToVersion == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "promote 必须给出 toVersion（晋升到哪一版）",
				"hint":  "GET /api/workflows/:id/versions 查版本号",
			})
			return
		}
		var ver model.WorkflowVersion
		if err := h.db.Where("workflow_id = ? AND version = ?", req.WorkflowID, req.ToVersion).
			First(&ver).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("该工作流没有版本 v%d", req.ToVersion),
				"hint":  "GET /api/workflows/:id/versions 看有哪些版本",
			})
			return
		}
		if req.Apply {
			if h.wf == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow 层未装配，拒绝写图"})
				return
			}
			// 与 rollback 同一条纪律：先快照再写图。没有快照就没有回滚路，
			// 而晋升写错图的代价（整条出片链路跑歪）比不晋升大得多。
			snapshot := h.wf.snapshotBefore(wf, -1)
			if snapshot == 0 {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "快照失败，图未改动 —— 没有快照就不晋升",
				})
				return
			}
			if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Updates(map[string]interface{}{
				"nodes": ver.Nodes,
				"edges": ver.Edges,
			}).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "写图失败：" + err.Error()})
				return
			}
			resp["applied"] = true
			resp["snapshotVersion"] = snapshot
			resp["hint"] = fmt.Sprintf(
				"v%d 已成为当前图（原图存为 v%d，要退回：POST /api/evolution/rollback {toVersion:%d, apply:true}）",
				req.ToVersion, snapshot, snapshot)
		} else {
			resp["applied"] = false
			resp["hint"] = "dry-run：只落台账不改图。确认后加 \"apply\": true 把 v" +
				strconv.Itoa(req.ToVersion) + " 写成当前图（会先快照）"
		}
	} else {
		// keep：只落台账，不动图
		resp["applied"] = false
		resp["hint"] = "已记录「保留基线」。图未改动"
	}

	if h.promos == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "晋升台账未装配（db 为 nil）"})
		return
	}
	if err := h.promos.Record(rec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "台账落库失败：" + err.Error()})
		return
	}
	resp["recorded"] = true
	resp["next"] = "晋升后要盯回归：GET /api/evolution/monitor?workflowId=" +
		fmt.Sprint(req.WorkflowID) + "（退化会给出该滚回的版本）"
	c.JSON(http.StatusOK, resp)
}

// Promotions GET /api/evolution/promotions?workflowId=N&limit=N
//
// 台账查询：这个工作流历史上所有的晋升/保留/回滚/引擎归档，新的在前。
func (h *EvolutionHandler) Promotions(c *gin.Context) {
	wfID := uint(0)
	if v := strings.TrimSpace(c.Query("workflowId")); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必须是正整数"})
			return
		}
		wfID = uint(n)
	}
	if wfID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填：台账按工作流查"})
		return
	}
	limit := 0
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if h.promos == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "晋升台账未装配（db 为 nil）"})
		return
	}
	recs := h.promos.List(wfID, limit)
	latest, has := h.promos.Latest(wfID)
	resp := gin.H{
		"workflowId": wfID,
		"count":      len(recs),
		"records":    recs,
	}
	if has {
		resp["latest"] = latest
	}
	c.JSON(http.StatusOK, resp)
}

// recordDecision  decide 的结论归档（record=true 时调用）。
// 归档失败只警告不阻断：判定结论本身已经返回给调用方了，
// 台账是旁路，不能因为它把主接口变成 500。
func (h *EvolutionHandler) recordDecision(c *gin.Context, workflowID uint, d service.Decision) {
	if h.promos == nil {
		return
	}
	reason := fmt.Sprintf("decide 归档：verdict=%s，%d 对配对（%d 胜 / %d 负 / %d 平），门分差 %+.4f",
		d.Verdict, d.Pairs, d.Better, d.Worse, d.Tie, d.GateScoreDelta)
	if len(d.Reasons) > 0 {
		reason += "；" + strings.Join(d.Reasons, "；")
	}
	if err := h.promos.Record(service.FromDecision(workflowID, d, reason)); err != nil {
		c.Header("X-Promotion-Warn", "decide 归档失败："+err.Error())
	}
}

// recordRegression monitor 的结论归档（record=true 时调用）。同上：旁路不阻断。
func (h *EvolutionHandler) recordRegression(c *gin.Context, workflowID uint, rep service.RegressionReport) {
	if h.promos == nil {
		return
	}
	reason := fmt.Sprintf("monitor 归档：verdict=%s", rep.Verdict)
	if len(rep.Reasons) > 0 {
		reason += "；" + strings.Join(rep.Reasons, "；")
	}
	rec := service.FromRegression(workflowID, rep, reason)
	if err := h.promos.Record(rec); err != nil {
		c.Header("X-Promotion-Warn", "monitor 归档失败："+err.Error())
	}
}

// recordRollback 回滚落库。回滚是显式动作，且已经改完图，
// 台账必须记下"从哪版滚到哪版、凭什么"——否则回滚本身就是不可审计的。
func (h *EvolutionHandler) recordRollback(workflowID uint, fromVersion, toVersion int, reason, actor string) error {
	if h.promos == nil {
		return fmt.Errorf("晋升台账未装配（db 为 nil）")
	}
	if strings.TrimSpace(reason) == "" {
		reason = fmt.Sprintf("回滚到 v%d", toVersion)
	}
	rec := model.Promotion{
		WorkflowID:  workflowID,
		Action:      model.PromotionActionRollback,
		Source:      model.PromotionSourceRollback,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Reason:      reason,
		Actor:       actor,
	}
	return h.promos.Record(rec)
}
