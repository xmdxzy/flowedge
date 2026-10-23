package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 闭环的另外四步：接尺子 / 跑配对 / 提变体 / 盯回归。
//
//	POST /api/evolution/instrument   给没有门的图接一条质检门（默认 dry-run）
//	POST /api/evolution/evaluate     按 case 列表 + 指定版本图跑配对（异步、串行）
//	GET  /api/evolution/batch        一批评测的进度与结果
//	POST /api/evolution/variants     文本梯度：模型读归因产出变体（已过审计）
//	GET  /api/evolution/monitor      晋升后回归体检
//	POST /api/evolution/rollback     回滚到某个版本（先快照，可再滚回来）
//
// 与既有四个端点合起来，闭环才闭合：
// 接尺子 → 跑配对 → 归因 → 提变体 → 审计 → 判定 → 晋升（人按）→ 盯回归 → 回滚
// 人按的每一次判定都落晋升台账（见 evolution_promotion.go），可审计。

// Instrument POST /api/evolution/instrument
//
// body: { workflowId, apply:false, tolerance:0.05 }
//
// 默认 dry-run：接门是改这张图的行为（从"跑完就算成"变成"过门才算成"），
// 必须让人先看清要插什么、阈值从哪推出来，再决定写不写。
func (h *EvolutionHandler) Instrument(c *gin.Context) {
	var req struct {
		WorkflowID uint    `json:"workflowId"`
		Apply      bool    `json:"apply"`
		Tolerance  float64 `json:"tolerance"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
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

	plan := service.InstrumentGates(nodes, edges, service.InstrumentOptions{Tolerance: req.Tolerance})
	resp := gin.H{
		"workflowId": wf.ID,
		"applied":    false,
		"wouldApply": plan.Applied,
		"gates":      plan.Gates,
		"derived":    plan.Derived,
		"skipped":    plan.Skipped,
	}
	if !plan.Applied {
		resp["verdict"] = "skipped"
		c.JSON(http.StatusOK, resp)
		return
	}

	// 结构守卫：接门不能把一张能跑的图接成跑不动的图
	before := service.ScoreGraph(nodes, edges)
	after := service.ScoreGraph(plan.Nodes, plan.Edges)
	resp["scoreBefore"] = before
	resp["scoreAfter"] = after
	if before.Legal && !after.Legal {
		resp["verdict"] = "rejected"
		resp["error"] = "接门后图从可执行变成不可执行，已拒绝"
		resp["hint"] = "看 scoreAfter 的 portMismatches / contractMissing：多半是终点前那条边本身就有问题"
		c.JSON(http.StatusConflict, resp)
		return
	}

	if !req.Apply {
		resp["verdict"] = "dry-run"
		resp["hint"] = "确认无误后加 \"apply\": true 落图（会先存快照，可回滚）"
		c.JSON(http.StatusOK, resp)
		return
	}

	if h.wf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow 层未装配，拒绝写图"})
		return
	}
	// 快照在前：旧图必须永远可回滚（与 /evidence/trial 同一条顺序纪律）
	snapshot := h.wf.snapshotBefore(wf, len(plan.Gates))
	if snapshot == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "快照失败，图未改动 —— 没有快照就没有回滚路",
		})
		return
	}
	nodesJSON, _ := json.Marshal(plan.Nodes)
	edgesJSON, _ := json.Marshal(plan.Edges)
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Updates(map[string]interface{}{
		"nodes": json.RawMessage(nodesJSON),
		"edges": json.RawMessage(edgesJSON),
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写图失败：" + err.Error()})
		return
	}
	resp["applied"] = true
	resp["verdict"] = "instrumented"
	resp["snapshotVersion"] = snapshot
	resp["hint"] = fmt.Sprintf(
		"已接门（回滚用版本 %d）。下一次 POST /api/workflows/%d/run 会带上门；跑完用 /api/evolution/status 看归因",
		snapshot, wf.ID)
	c.JSON(http.StatusOK, resp)
}

// EvaluateCase 一个评测用例
type EvaluateCase struct {
	CaseID   string `json:"caseId"`
	Category string `json:"category"`
}

// Evaluate POST /api/evolution/evaluate
//
// body: { workflowId, version?:int, role:"baseline"|"candidate", cases:[{caseId,category}],
//
//	datasetFingerprint?, note? }
//
// 三件事必须说清：
//   - 跑的是**哪一版图**：version>0 时读历史版本，用内存图跑，**不动当前图**
//     （否则每做一次对比都要来回回滚，回滚本身又会产生新的不确定性）
//   - 是**哪一侧**：role 写进 run，证据信封据此配对；没有 role 的证据不参与判定
//   - **异步**：一次出片是分钟级，接口只负责受理。串行由队列信号量天然保证。
func (h *EvolutionHandler) Evaluate(c *gin.Context) {
	var req struct {
		WorkflowID         uint           `json:"workflowId"`
		Version            int            `json:"version"`
		Role               string         `json:"role"`
		Cases              []EvaluateCase `json:"cases"`
		DatasetFingerprint string         `json:"datasetFingerprint"`
		Note               string         `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 || len(req.Cases) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 与 cases 都必填"})
		return
	}
	req.Role = strings.TrimSpace(req.Role)
	if req.Role != "baseline" && req.Role != "candidate" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "role 必须是 baseline 或 candidate —— 两侧写反会让配对判定得出完全相反的结论",
		})
		return
	}
	if len(req.Cases) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "一次最多 50 个 case：出片是分钟级，别把队列排到明天"})
		return
	}
	for i := range req.Cases {
		req.Cases[i].CaseID = strings.TrimSpace(req.Cases[i].CaseID)
		if req.Cases[i].CaseID == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("第 %d 个 case 缺 caseId", i+1),
				"hint":  "caseId 是配对的主键：没有它，改前改后分不清谁对谁",
			})
			return
		}
	}

	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	if h.wf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow 层未装配，无法起跑"})
		return
	}

	// 选图：显式版本 > 当前图
	graphNodes, graphEdges := wf.Nodes, wf.Edges
	version := req.Version
	if version > 0 {
		var v model.WorkflowVersion
		if err := h.db.Where("workflow_id = ? AND version = ?", req.WorkflowID, version).First(&v).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("该工作流没有版本 v%d", version),
				"hint":  "GET /api/workflows/:id/versions 看有哪些版本",
			})
			return
		}
		graphNodes, graphEdges = v.Nodes, v.Edges
	} else {
		version = h.wf.latestVersion(req.WorkflowID)
	}

	batchID := fmt.Sprintf("eval-%d-%d", req.WorkflowID, time.Now().UnixNano())
	cases := append([]EvaluateCase(nil), req.Cases...)
	fingerprint := strings.TrimSpace(req.DatasetFingerprint)

	// 串行起跑：StartRunWith 内部 Enqueue 会被队列信号量挡住，
	// 所以这个 goroutine 天然一次只跑一个（单 GPU 不能并行出片）。
	go func() {
		for _, cs := range cases {
			h.wf.StartRunWith(&wf, RunOptions{
				InvokedBy:          "evaluate",
				CaseID:             cs.CaseID,
				Category:           cs.Category,
				DatasetFingerprint: fingerprint,
				Role:               req.Role,
				EvalBatch:          batchID,
				Nodes:              graphNodes,
				Edges:              graphEdges,
				WorkflowVersion:    version,
			})
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"batchId":    batchID,
		"accepted":   len(cases),
		"role":       req.Role,
		"version":    version,
		"workflowId": req.WorkflowID,
		"poll":       "/api/evolution/batch?batchId=" + batchID,
		"note": "已按 case 顺序串行起跑。跑完用 batch 看进度，再 POST /api/evolution/decide 出结论。" +
			"两侧都要跑：先 baseline（旧版图）再 candidate（新版图），同一个 caseId 才配得上",
	})
}

// Batch GET /api/evolution/batch?batchId=...
//
// 一批评测的进度。判据用**数值**（完成数 / 门分数），不看"跑没跑完"这种字符串。
func (h *EvolutionHandler) Batch(c *gin.Context) {
	batchID := strings.TrimSpace(c.Query("batchId"))
	if batchID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "batchId 必填"})
		return
	}
	var runs []model.WorkflowRun
	h.db.Where("eval_batch = ?", batchID).Order("id asc").Find(&runs)
	if len(runs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "没有这个批次（eval_batch 为空）"})
		return
	}

	ids := map[uint]bool{}
	for _, r := range runs {
		ids[r.ID] = true
	}
	evs, _, _ := service.LoadEvidenceFiles(service.EvidenceDir())
	byRun := map[uint]service.Evidence{}
	for _, ev := range evs {
		if ids[ev.RunID] {
			byRun[ev.RunID] = ev
		}
	}

	type row struct {
		RunID     uint     `json:"runId"`
		CaseID    string   `json:"caseId,omitempty"`
		Category  string   `json:"category,omitempty"`
		Status    string   `json:"status"`
		Result    string   `json:"result,omitempty"`
		GateScore *float64 `json:"gateScore"`
		Error     string   `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(runs))
	done, scored, passed := 0, 0, 0
	for _, r := range runs {
		rr := row{RunID: r.ID, Status: r.Status, Error: r.Error}
		if r.CaseID != nil {
			rr.CaseID = *r.CaseID
		}
		if r.Category != nil {
			rr.Category = *r.Category
		}
		if r.Status != "running" && r.Status != "pending" {
			done++
		}
		if ev, ok := byRun[r.ID]; ok {
			rr.Result = ev.Result
			rr.GateScore = ev.GateScore
			if ev.Result == service.EvidencePassed || ev.Result == service.EvidenceFailed {
				scored++
			}
			if ev.Result == service.EvidencePassed {
				passed++
			}
		}
		rows = append(rows, rr)
	}
	c.JSON(http.StatusOK, gin.H{
		"batchId": batchID,
		"total":   len(runs),
		"done":    done,
		"scored":  scored,
		"passed":  passed,
		"role":    runs[0].Role,
		"version": runs[0].WorkflowVersion,
		"runs":    rows,
		"note":    "scored 才算拿到分数；done 但 unscored 说明没测到（没接门/门崩了/执行挂了），不是不合格",
	})
}

// Variants POST /api/evolution/variants
//
// body: { workflowId, max:3, goal? }
//
// 文本梯度：模型读归因，提变体；引擎负责校正 from、摘掉改判据的编辑、跑审计。
// 产出的 accepted 变体可以直接喂 POST /api/evidence/trial 试。
func (h *EvolutionHandler) Variants(c *gin.Context) {
	var req struct {
		WorkflowID uint   `json:"workflowId"`
		Max        int    `json:"max"`
		Goal       string `json:"goal"`
		// MinPairs 与 decide / monitor / rollback 同口径
		MinPairs int `json:"minPairs"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	_, nodes, ok := h.loadWorkflow(c, req.WorkflowID)
	if !ok {
		return
	}
	policy := evoPolicy()
	if req.MinPairs > 0 {
		policy.MinPairs = req.MinPairs
	}
	evs := service.FilterEvidence(mustLoadEvidence(), req.WorkflowID)
	attrs := service.AttributeEvidence(evs, nodes)
	fuel := service.InspectFuel(evs, policy)

	// 燃料不够就别浪费一次模型调用：先补证据，再谈变体。
	if !fuel.Ready {
		c.JSON(http.StatusPreconditionFailed, gin.H{
			"error": "燃料不足，未生成变体（省一次模型调用，也避免在没有尺子时改图）",
			"fuel":  fuel,
			"hint":  "按 fuel.blockers 补齐：接门 → 注入 caseId/role → 两侧各跑到 ≥8 对",
		})
		return
	}
	var llm service.ComposeLLM
	if h.engine != nil {
		llm = h.engine.ComposeLLM()
	}
	variants, err := service.ProposeTextualVariants(service.VariantRequest{
		Nodes: nodes, Attrs: attrs, Max: req.Max, Prompt: req.Goal,
	}, llm)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "变体生成失败：" + err.Error(),
			"hint":  "文本梯度必须有一次模型调用；模型不可用时不许退化成「返回空清单」—— 那是另一个事实",
		})
		return
	}
	accepted := 0
	for _, v := range variants {
		if v.Verdict == "accepted" {
			accepted++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"workflowId":  req.WorkflowID,
		"count":       len(variants),
		"accepted":    accepted,
		"variants":    variants,
		"attribution": attrs,
		"note": "accepted 的变体可直接 POST /api/evidence/trial（edits 用 editsAsCandidate）；" +
			"试完必须双侧跑配对证据，再由 /api/evolution/decide 判定 —— 引擎不自动晋升",
	})
}

// Monitor GET /api/evolution/monitor?workflowId=N&record=true
//
// 晋升后回归体检：当前版本相对基线有没有退化，退化建议滚回哪一版。
// record=true 时把体检结论归档进晋升台账（action=archive）。
func (h *EvolutionHandler) Monitor(c *gin.Context) {
	var wfID uint
	if v := c.Query("workflowId"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			wfID = uint(n)
		}
	}
	if wfID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	record := strings.TrimSpace(c.Query("record")) == "true"
	policy := evoPolicy()
	if v := c.Query("minPairs"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			policy.MinPairs = n
		}
	}
	evs := service.FilterEvidence(mustLoadEvidence(), wfID)
	rep := service.MonitorRegression(evs, policy)

	if record {
		h.recordRegression(c, wfID, rep)
	}

	resp := gin.H{
		"workflowId":     wfID,
		"report":         rep,
		"currentVersion": h.latestVersionOf(wfID),
		"recorded":       record,
	}
	switch rep.Verdict {
	case "regressed":
		resp["next"] = "退化已确认。滚回去：POST /api/evolution/rollback {workflowId, toVersion: " +
			fmt.Sprint(rep.RollbackTo) + ", apply: true}（回滚前会再快照一次，滚错还能滚回来）"
	case "insufficient":
		resp["next"] = "证据不足以下结论：先补齐两侧配对（baseline/candidate + caseId），别按「看起来差不多」判断"
	default:
		resp["next"] = "没有退化迹象；继续保持同一 case 集的周期评测，退化才看得出来"
	}
	c.JSON(http.StatusOK, resp)
}

// Rollback POST /api/evolution/rollback
//
// body: { workflowId, toVersion, apply:false }
//
// 与 /workflows/:id/versions/:versionId/rollback 的区别：这里按**版本号**回滚、
// 回滚前强制快照当前状态（让回滚本身可撤销），且默认 dry-run。
func (h *EvolutionHandler) Rollback(c *gin.Context) {
	var req struct {
		WorkflowID uint `json:"workflowId"`
		ToVersion  int  `json:"toVersion"`
		Apply      bool `json:"apply"`
		// MinPairs 与 decide / monitor 同口径：判定阈值必须能被显式设定，
		// 否则小数据集（如自检用的 2 对）永远判不出退化，回滚路径也就无从验证。
		MinPairs int `json:"minPairs"`
		// Reason / Actor 落晋升台账用：回滚是显式动作，
		// 「从哪版滚到哪版、凭什么」必须可审计（不填按「回滚到 vN」记）。
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	// toVersion 不给就按监测结论取：只回滚到证据支持的那一版，不猜"上一版"
	if req.ToVersion == 0 {
		policy := evoPolicy()
		if req.MinPairs > 0 {
			policy.MinPairs = req.MinPairs
		}
		rep := service.MonitorRegression(
			service.FilterEvidence(mustLoadEvidence(), req.WorkflowID), policy)
		if rep.Verdict != "regressed" || rep.RollbackTo == 0 {
			c.JSON(http.StatusPreconditionFailed, gin.H{
				"error":  "没有可回滚的结论：监测既没说退化，也没给出基线版本",
				"report": rep,
				"hint":   "要强制回滚请显式传 toVersion（GET /api/workflows/:id/versions 查版本号）",
			})
			return
		}
		req.ToVersion = rep.RollbackTo
	}

	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	var ver model.WorkflowVersion
	if err := h.db.Where("workflow_id = ? AND version = ?", req.WorkflowID, req.ToVersion).
		First(&ver).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("该工作流没有版本 v%d", req.ToVersion)})
		return
	}

	var fromNodes, toNodes []model.NodeInstance
	_ = json.Unmarshal(wf.Nodes, &fromNodes)
	_ = json.Unmarshal(ver.Nodes, &toNodes)
	resp := gin.H{
		"workflowId": req.WorkflowID,
		"toVersion":  req.ToVersion,
		"nodeCount":  gin.H{"from": len(fromNodes), "to": len(toNodes)},
	}
	if !req.Apply {
		resp["applied"] = false
		resp["hint"] = "加 \"apply\": true 执行回滚（会先给当前图存一个快照）"
		c.JSON(http.StatusOK, resp)
		return
	}
	if h.wf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow 层未装配，拒绝写图"})
		return
	}
	snapshot := h.wf.snapshotBefore(wf, -1)
	if snapshot == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "快照失败，未回滚 —— 没有快照就没有第二次回滚"})
		return
	}
	if err := h.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Updates(map[string]interface{}{
		"nodes": ver.Nodes,
		"edges": ver.Edges,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "回滚写图失败：" + err.Error()})
		return
	}
	resp["applied"] = true
	resp["snapshotVersion"] = snapshot
	// 台账落库：图已经改完了，记录必须跟着落。fromVersion 用快照号 ——
	// 快照存的正是回滚前的状态，"从哪版滚"有据可查，不用猜。
	// 归档失败只警告不翻账：回滚本身已完成，把成功响应改成 500
	// 会让人以为没滚，反而再滚一次。
	if err := h.recordRollback(req.WorkflowID, snapshot, req.ToVersion, req.Reason, req.Actor); err != nil {
		resp["promotionWarn"] = "回滚成功但台账落库失败：" + err.Error()
	}
	resp["hint"] = fmt.Sprintf(
		"已回滚到 v%d。当前状态存为 v%d：滚错了 POST /api/evolution/rollback {toVersion:%d, apply:true} 即可退回",
		req.ToVersion, snapshot, snapshot)
	c.JSON(http.StatusOK, resp)
}

// latestVersionOf 当前最新快照版本号（0 = 没有快照）
func (h *EvolutionHandler) latestVersionOf(workflowID uint) int {
	var v int
	h.db.Model(&model.WorkflowVersion{}).Where("workflow_id = ?", workflowID).
		Select("COALESCE(MAX(version), 0)").Scan(&v)
	return v
}
