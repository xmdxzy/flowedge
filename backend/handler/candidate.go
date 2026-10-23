package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 候选生成与试验。
//
// 这一步把 D4 从「诊断」推到「能试」：
//
//	GET  /api/evidence/summary            哪里在漏
//	POST /api/evidence/candidates         能改成什么（有标准答案的才提）
//	POST /api/evidence/trial              改了会不会更好（快照 + 可选重跑）
//
// 为什么 trial 默认**不自动跑**：出片成本是分钟到小时级，且会占显存。
// "一键"应该省掉的是复制粘贴与手工改图，不是替你决定花 40 分钟渲染。
// 想连跑就显式传 run=true —— 明确表达意图，而不是默认很激进。

// Candidates POST /api/evidence/candidates
//
// body: { workflowId, diagnosis }
// 只读：从图里读当前值，生成候选清单，不改任何东西。
func (h *EvidenceHandler) Candidates(c *gin.Context) {
	var req struct {
		WorkflowID uint              `json:"workflowId"`
		Diagnosis  service.Diagnosis `json:"diagnosis"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 || req.Diagnosis.Kind == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 与 diagnosis 都必填（diagnosis 从 /api/evidence/summary 原样取）"})
		return
	}

	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	var nodes []model.NodeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "节点数据损坏：" + err.Error()})
		return
	}

	cands := service.ProposeCandidates(req.Diagnosis, wf.ID, wf.Name, nodes)
	note := ""
	if len(cands) == 0 {
		note = "这条诊断没有可自动提议的参数改动。" +
			"环境类（不可测/长尾）与结构类问题改参数只会掩盖症状 —— 按诊断里的 suggestion 去修根因"
	}
	c.JSON(http.StatusOK, gin.H{"candidates": cands, "note": note})
}

// Trial POST /api/evidence/trial
//
// body: { workflowId, edits:[{nodeId,path,from,to,why}], run:false, note:"…" }
//
// 做四件事，任何一步不过就整批不动：
//  1. 校验编辑（复用 spec/apply 的四条硬拦，dry-run 不落盘）
//  2. 结构评分对比：改完不能比改前更不可执行
//  3. 快照当前状态，写入新图
//  4. 可选：立即起跑一次
func (h *EvidenceHandler) Trial(c *gin.Context) {
	var req struct {
		WorkflowID uint                    `json:"workflowId"`
		Edits      []service.CandidateEdit `json:"edits"`
		Run        bool                    `json:"run"`
		Note       string                  `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 || len(req.Edits) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 与 edits 都必填"})
		return
	}

	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "节点数据损坏：" + err.Error()})
		return
	}
	_ = json.Unmarshal(wf.Edges, &edges)

	specEdits := make([]service.SpecEdit, 0, len(req.Edits))
	for _, e := range req.Edits {
		specEdits = append(specEdits, service.SpecEdit{
			NodeID: e.NodeID, Path: e.Path, From: e.From, To: e.To, Reason: e.Why,
		})
	}

	// ① 校验：复用 spec/apply 的硬拦（只允许 data.*、from 必须与现场一致…）
	//
	// AllowGateChange 必须为 true：候选要调的绝大多数参数（loudness/fps/max_silence_gap）
	// 本身就是门层参数。这不是绕过门锁 —— 门锁的用意是"改门必须留名"，
	// 而候选的每一条都带 Why（写在 Reason 里），正好满足这个要求。
	// 直接调 /spec/apply 时仍然是拦的（有专门用例守着）。
	next, applied, probs := service.ApplySpecEdits(nodes, specEdits, service.ApplyOptions{AllowGateChange: true})
	if len(probs) > 0 {
		c.JSON(worstStatus(probs), gin.H{
			"error":    "候选未通过校验，图未改动",
			"problems": probs,
		})
		return
	}

	// ② 结构守卫：改参数不能把一张能跑的图改坏
	before := service.ScoreGraph(nodes, edges)
	after := service.ScoreGraph(next, edges)
	if before.Legal && !after.Legal {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "这批改动会让图从可执行变成不可执行，已拒绝",
			"before": before,
			"after":  after,
			"hint":   "先看 after 的 portMismatches / contractMissing",
		})
		return
	}

	// ③ 快照 + 写入（快照在前：旧图必须永远可回滚）
	versionBefore := h.wf.latestVersion(wf.ID)
	snapshot := h.wf.snapshotBefore(wf, len(applied))
	nodesJSON, err := json.Marshal(next)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化失败"})
		return
	}
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).
		Update("nodes", json.RawMessage(nodesJSON)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写回失败：" + err.Error()})
		return
	}
	versionAfter := h.wf.latestVersion(wf.ID)

	resp := gin.H{
		"applied":         applied,
		"count":           len(applied),
		"versionBefore":   versionBefore,
		"snapshotVersion": snapshot,
		"versionAfter":    versionAfter,
		"scoreBefore":     before.Score,
		"scoreAfter":      after.Score,
		"note":            req.Note,
	}

	// ④ 可选起跑。不跑时把"下一步"写清楚，别让人猜。
	if !req.Run {
		resp["hint"] = fmt.Sprintf("图已改（回滚可用版本 %d）。要验证效果就跑一次：POST /api/workflows/%d/run，或在本次请求里加 \"run\": true", snapshot, wf.ID)
		c.JSON(http.StatusOK, resp)
		return
	}

	var fresh model.Workflow
	if err := h.db.First(&fresh, wf.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重读工作流失败"})
		return
	}
	if misses := h.wf.preflight(fresh.Nodes, fresh.Edges); len(misses) > 0 {
		resp["preflightMissing"] = misses
		resp["hint"] = "图已改，但依赖服务没就绪，未起跑"
		c.JSON(http.StatusOK, resp)
		return
	}
	run := h.wf.StartRun(&fresh, "manual")
	resp["runId"] = run.ID
	resp["hint"] = "已起跑。跑完用 /api/evidence/summary 看这批（version=" + itoa(versionAfter) + "）的门级结果"
	c.JSON(http.StatusAccepted, resp)
}

// SetWorkflowHandler 注入 workflow handler（复用 preflight / StartRun / 快照，不复制一套）
func (h *EvidenceHandler) SetWorkflowHandler(w *WorkflowHandler) { h.wf = w }
