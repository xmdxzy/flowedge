package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 自进化闭环的 HTTP 出口。
//
// 五个端点对应闭环的五个环节，顺序就是它们该被调用的顺序：
//
//	GET  /api/evolution/status    现在漏在哪、燃料够不够（只读）
//	POST /api/evolution/propose   该改哪一处（只读，产候选不落盘）
//	POST /api/evolution/audit     这个改法是不是在改评分标准（只读，dry-run）
//	   —— 真跑用已有的 POST /api/evidence/trial（含快照 + 结构守卫 + 可选取跑）
//	POST /api/evolution/decide    改了到底有没有更好（读 evidence，配对判定）
//	报告/晋升由人确认：引擎只给结论，不自己按下晋升。
//
// 为什么最后一步不给自动晋升：FlowEdge 的 reward 与 agent 在同一套文件里，
// 判定链路再严也只是"规则更严的自我评价"。让它自己晋升，等于让考生自己改分数册。
// 引擎能做的是**把结论做得足够硬，硬到人只需要看一眼**。
type EvolutionHandler struct {
	db *gorm.DB
	// engine 只为文本梯度要一次模型调用（复用 ComposeLLM 的 deterministicChat：
	// 变体生成要可复现，同一份归因重复调用应当给出同一批候选）
	engine *service.EngineV2
	// wf 复用 workflow handler 的快照与版本逻辑，不另写一套写图路径
	wf *WorkflowHandler
	// promos 晋升台账：人按下的每次判定都落库，可审计
	promos *service.PromotionStore
}

func NewEvolutionHandler(db *gorm.DB, engine *service.EngineV2) *EvolutionHandler {
	return &EvolutionHandler{db: db, engine: engine, promos: service.NewPromotionStore(db)}
}

// SetWorkflowHandler 注入 workflow handler（快照 / 版本 / 起跑复用）
func (h *EvolutionHandler) SetWorkflowHandler(w *WorkflowHandler) { h.wf = w }

// evoPolicy 允许环境变量微调，但默认值与序策对齐
func evoPolicy() service.EvoPolicy {
	p := service.DefaultEvoPolicy()
	if v := strings.TrimSpace(os.Getenv("FLOWEDGE_EVO_MIN_PAIRS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			p.MinPairs = n
		}
	}
	return p
}

// Status GET /api/evolution/status?workflowId=N
//
// 一次返回「燃料够不够 + 漏在哪 + 可调面有多大」。
// 燃料不足时 attribution 照样给（那是事实），但 next 会明确说"先补数据、别改参数"。
func (h *EvolutionHandler) Status(c *gin.Context) {
	var wfID uint
	if v := c.Query("workflowId"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			wfID = uint(n)
		}
	}
	policy := evoPolicy()

	all, bad, err := service.LoadEvidenceFiles(service.EvidenceDir())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读证据目录失败：" + err.Error()})
		return
	}
	evs := service.FilterEvidence(all, wfID)
	fuel := service.InspectFuel(evs, policy)

	resp := gin.H{
		"workflowId":  wfID,
		"policy":      policy,
		"fuel":        fuel,
		"evidenceDir": service.EvidenceDir(),
		"badFiles":    bad,
	}
	if bad > 0 {
		resp["warn"] = fmt.Sprintf("%d 个证据文件读不了（JSON 损坏）—— 样本量比实际少，别按 healthy 读", bad)
	}

	// 归因需要图；没有 workflowId 就给全库就绪度，不猜图
	if wfID == 0 {
		resp["readiness"] = h.readiness()
		resp["next"] = "给 workflowId 才能把失分归因到具体节点与可调旋钮"
		c.JSON(http.StatusOK, resp)
		return
	}
	wf, nodes, ok := h.loadWorkflow(c, wfID)
	if !ok {
		return
	}
	_ = wf

	attrs := service.AttributeEvidence(evs, nodes)
	surface := service.TrainableSurface(nodes)
	locked, related := 0, 0
	for _, k := range surface {
		if k.GateLocked {
			locked++
		}
		if k.GateRelated {
			related++
		}
	}
	resp["attribution"] = attrs
	resp["surface"] = gin.H{
		"total":   len(surface),
		"locked":  locked,  // 门上的判据，永不自动调
		"related": related, // 门层词汇的产出参数，可调但要留名
		"knobs":   surface,
	}

	switch {
	case !fuel.Ready:
		resp["next"] = "燃料不足：先按 fuel.blockers 补齐，再谈改参数。现在改参数属于在没有刻度尺的情况下调焦"
	case len(attrs) == 0:
		resp["next"] = "门全过，没有可归因的失分。要继续进化得换更严的 case 集（当前 case 已经测不出差别）"
	default:
		resp["next"] = "POST /api/evolution/propose 拿候选 → audit 查是否作弊 → trial 真跑 → decide 判定"
	}
	c.JSON(http.StatusOK, resp)
}

// Propose POST /api/evolution/propose
//
// body: { workflowId, minLossShare? }
// 只读：把归因里 param_tunable 的项翻成具体候选（复用 candidate.go 的规则与取值）。
func (h *EvolutionHandler) Propose(c *gin.Context) {
	var req struct {
		WorkflowID   uint    `json:"workflowId"`
		MinLossShare float64 `json:"minLossShare"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	wf, nodes, ok := h.loadWorkflow(c, req.WorkflowID)
	if !ok {
		return
	}
	evs := service.FilterEvidence(mustLoadEvidence(), wf.ID)
	attrs := service.AttributeEvidence(evs, nodes)

	var cands []service.TrialCandidate
	var skipped []gin.H
	var stripped []gin.H
	for _, a := range attrs {
		if a.Verdict != "param_tunable" {
			if a.Failed > 0 && (req.MinLossShare <= 0 || a.LossShare >= req.MinLossShare) {
				skipped = append(skipped, gin.H{
					"gateCheck": a.GateCheck, "verdict": a.Verdict, "reason": a.Reason,
				})
			}
			continue
		}
		if req.MinLossShare > 0 && a.LossShare < req.MinLossShare {
			continue
		}
		diag := service.Diagnosis{Kind: "gate_weak", Target: wf.Name + " / " + a.GateCheck}
		for _, c := range service.ProposeCandidates(diag, wf.ID, wf.Name, nodes) {
			// 摘掉改判据的编辑：规则表为了"整体对齐口径"会把门下限一起改，
			// 自进化路径上那等于自己改尺子。摘掉的如实报出来。
			kept, dropped := service.FilterGateJudgementEdits(nodes, c.Edits)
			for _, d := range dropped {
				stripped = append(stripped, gin.H{
					"candidate": c.Title, "nodeId": d.NodeID, "path": d.Path,
					"reason": "这是门判据，自动调它等于改评分标准",
				})
			}
			if len(kept) == 0 {
				skipped = append(skipped, gin.H{
					"gateCheck": a.GateCheck, "verdict": "gate_side",
					"reason": "这条检查对应的改动全在门判据上，没有可自动调的产出参数",
				})
				continue
			}
			c.Edits = kept
			cands = append(cands, c)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"candidates": cands,
		"count":      len(cands),
		"skipped":    skipped,
		"stripped":   stripped,
		"note": "只提有行业标准值的产出参数。提示词/模型选择/风格没有标准答案，不在这里生成 —— " +
			"那类改动必须由人或有依据的外部 agent 提假设，再配对验证；" +
			"改门判据的编辑一律摘掉并记在 stripped 里",
	})
}

// Audit POST /api/evolution/audit
//
// body: { workflowId, edits:[{nodeId,path,from,to,why}] }
// 只读 dry-run：不改图，只回答三件事——
//  1. 这批改动是不是在改评分标准（hacks）
//  2. 至少要跑到哪一级才能验证（projection）
//  3. 改完图还可执行吗（结构分）
func (h *EvolutionHandler) Audit(c *gin.Context) {
	var req struct {
		WorkflowID uint `json:"workflowId"`
		Edits      []struct {
			NodeID string      `json:"nodeId"`
			Path   string      `json:"path"`
			From   interface{} `json:"from"`
			To     interface{} `json:"to"`
			// Why 与 Reason 是同一个意思：候选清单里叫 why（人话），
			// spec/apply 里叫 reason（审计字段）。两个都收，免得调用方为了字段名翻一次文档。
			Why    string `json:"why"`
			Reason string `json:"reason"`
		} `json:"edits"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 || len(req.Edits) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 与 edits 都必填"})
		return
	}
	edits := make([]service.SpecEdit, 0, len(req.Edits))
	for _, e := range req.Edits {
		reason := e.Reason
		if strings.TrimSpace(reason) == "" {
			reason = e.Why
		}
		edits = append(edits, service.SpecEdit{
			NodeID: e.NodeID, Path: e.Path, From: e.From, To: e.To, Reason: reason,
		})
	}
	_, nodes, ok := h.loadWorkflow(c, req.WorkflowID)
	if !ok {
		return
	}
	var wf model.Workflow
	if err := h.db.First(&wf, req.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	var edges []model.EdgeInstance
	_ = json.Unmarshal(wf.Edges, &edges)

	next, applied, probs := service.ApplySpecEdits(nodes, edits, service.ApplyOptions{
		AllowGateChange: true, // dry-run 放行，真正的拦截在下面的 AuditVariant
		DryRun:          true,
	})
	resp := gin.H{
		"applied":    applied,
		"problems":   probs,
		"projection": service.ProjectionFor(nodes, edits),
	}
	if len(probs) > 0 {
		resp["verdict"] = "rejected"
		resp["error"] = "编辑本身不成立（from 与现场不一致 / 路径越界）"
		c.JSON(http.StatusConflict, resp)
		return
	}

	hacks, warnings := service.AuditVariant(nodes, next)
	before := service.ScoreGraph(nodes, edges)
	after := service.ScoreGraph(next, edges)
	resp["hacks"] = hacks
	resp["warnings"] = warnings
	resp["scoreBefore"] = before
	resp["scoreAfter"] = after
	resp["legalAfter"] = after.Legal

	switch {
	case len(hacks) > 0:
		resp["verdict"] = "rejected"
		resp["reason"] = "这批改动里有改评分标准的项。放宽验收的收益永远大于提高质量，" +
			"一旦放行，进化会稳定收敛到最松的门"
	case before.Legal && !after.Legal:
		resp["verdict"] = "rejected"
		resp["reason"] = "改完图从可执行变成不可执行"
	default:
		resp["verdict"] = "pass"
		if resp["projection"] == service.ProjectionReal {
			resp["reason"] = "改动会改变渲染成本，必须真跑一次才能验证（分钟级、占显存）"
		} else {
			resp["reason"] = "改动不改变渲染成本，可先干跑；但「不改变成本」不等于「有改善」，仍须配对证据"
		}
	}
	c.JSON(http.StatusOK, resp)
}

// Decide POST /api/evolution/decide
//
// body: { workflowId, minPairs?, record? }
// 读该工作流的 evidence，按 Role 分成 baseline / candidate 做配对判定。
// 只给结论，不改图 —— 晋升是人按下去的（POST /api/evolution/promote）。
// record=true 时把结论归档进晋升台账（action=archive，人未按键），供事后审计。
func (h *EvolutionHandler) Decide(c *gin.Context) {
	var req struct {
		WorkflowID uint `json:"workflowId"`
		MinPairs   int  `json:"minPairs"`
		Record     bool `json:"record"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if req.WorkflowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflowId 必填"})
		return
	}
	policy := evoPolicy()
	if req.MinPairs > 0 {
		policy.MinPairs = req.MinPairs
	}
	evs := service.FilterEvidence(mustLoadEvidence(), req.WorkflowID)

	var base, cand []service.Evidence
	for _, ev := range evs {
		switch ev.Role {
		case "baseline":
			base = append(base, ev)
		case "candidate":
			cand = append(cand, ev)
		}
	}
	d := service.Decide(base, cand, policy)
	fuel := service.InspectFuel(evs, policy)

	if req.Record {
		h.recordDecision(c, req.WorkflowID, d)
	}

	c.JSON(http.StatusOK, gin.H{
		"decision": d,
		"fuel":     fuel,
		"recorded": req.Record,
		"input":    gin.H{"baseline": len(base), "candidate": len(cand), "total": len(evs)},
		"note": "role 由评测层写入（baseline / candidate）；" +
			"缺 role 或 caseId 的运行不会进入配对 —— 按时间顺序配会得出错误结论",
	})
}

// readiness 全库就绪度：有多少条工作流真的接了门。
//
// 这是「能不能自进化」的第一道体检，也是最容易被跳过的一问：
// 讨论算法（RL / 文本梯度 / 帕累托）之前，先看**有没有门在产生分数**。
// 一条门都没有的库，任何自进化算法都只会把随机改动包装成"优化"。
func (h *EvolutionHandler) readiness() gin.H {
	var wfs []model.Workflow
	if err := h.db.Select("id", "name", "nodes").Find(&wfs).Error; err != nil {
		return gin.H{"error": "读工作流失败：" + err.Error()}
	}
	var withGate, withTransform int
	var gated []gin.H
	for _, wf := range wfs {
		var nodes []model.NodeInstance
		if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
			continue
		}
		gates, knobs := 0, 0
		for i := range nodes {
			mod, ok := service.ModuleContractOf(nodes[i].Type)
			if !ok {
				continue
			}
			if mod.Role == service.RoleGate {
				gates++
			}
		}
		knobs = len(service.TrainableSurface(nodes))
		if gates > 0 {
			withGate++
			gated = append(gated, gin.H{"id": wf.ID, "name": wf.Name, "gates": gates, "knobs": knobs})
		}
		if knobs > 0 {
			withTransform++
		}
	}
	rate := 0.0
	if len(wfs) > 0 {
		rate = float64(withGate) / float64(len(wfs))
	}
	out := gin.H{
		"workflows":      len(wfs),
		"withGate":       withGate,
		"withParams":     withTransform,
		"gateCoverage":   rate,
		"gatedWorkflows": gated,
	}
	if withGate == 0 {
		out["verdict"] = "零门：自进化无燃料。任何「优化」都无法验证，先把 quality_gate 接进主流程"
	} else if rate < 0.5 {
		out["verdict"] = "门覆盖率不到一半：能进化的只有那几条图，其余改动全是盲改"
	} else {
		out["verdict"] = "有门可依，可以开始配对评测"
	}
	return out
}

// loadWorkflow 读图，失败时已写好响应
func (h *EvolutionHandler) loadWorkflow(c *gin.Context, id uint) (model.Workflow, []model.NodeInstance, bool) {
	var wf model.Workflow
	if err := h.db.First(&wf, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return wf, nil, false
	}
	var nodes []model.NodeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "节点数据损坏：" + err.Error()})
		return wf, nil, false
	}
	return wf, nodes, true
}

// mustLoadEvidence 读不到就当空（进化是旁路，不能因为它把主接口打挂）
func mustLoadEvidence() []service.Evidence {
	evs, _, err := service.LoadEvidenceFiles(service.EvidenceDir())
	if err != nil {
		return nil
	}
	return evs
}
