package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// Producer 出片入口：选流水线 + 填字段 → 服务端组装固定图 → 起跑。
//
// 这是 FlowEdge 收敛后**唯一**的运行入口。原来那条
// 「画布 → 保存工作流 → 点运行」的路已经去掉：图不再由用户画。
type Producer struct {
	db     *gorm.DB
	engine *service.EngineV2
	queue  *service.JobQueue
	pipes  *service.PipelineStore

	// 依赖预检（SetPreflight 注入；nil 时跳过）
	sup         *service.Supervisor
	ollamaBase  string
	comfyBase   string
	comfyH3Base string
	asrBase     string
	ttsBase     string

	// 质量循环的默认轮数（qc=true 时生效）
	qcRounds int

	mu sync.Mutex
}

func NewProducer(db *gorm.DB, engine *service.EngineV2, queue *service.JobQueue, pipes *service.PipelineStore) *Producer {
	return &Producer{db: db, engine: engine, queue: queue, pipes: pipes, qcRounds: 3}
}

// SetPreflight 注入依赖探测所需端点。
func (p *Producer) SetPreflight(sup *service.Supervisor, ollamaBase, comfyBase, comfyH3Base, asrBase, ttsBase string) {
	p.sup = sup
	p.ollamaBase = ollamaBase
	p.comfyBase = comfyBase
	p.comfyH3Base = comfyH3Base
	p.asrBase = asrBase
	p.ttsBase = ttsBase
}

// SetQCRounds 质量循环轮数（1-8），0 或超界回落默认 3。
func (p *Producer) SetQCRounds(n int) {
	if n > 0 && n <= 8 {
		p.qcRounds = n
	}
}

// SweepTransient 清掉上次残留的质量循环临时图（进程崩溃会留下）。
func (p *Producer) SweepTransient() int {
	res := p.db.Where("name LIKE ?", "qc:%").Delete(&model.Workflow{})
	return int(res.RowsAffected)
}

// ListPipelines 返回向导要用的流水线目录。
func (p *Producer) ListPipelines(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"pipelines": p.pipes.List(),
		"count":     len(p.pipes.List()),
	})
}

// ReloadPipelines 热重载 data/pipelines.json（改完不用重启）。
func (p *Producer) ReloadPipelines(c *gin.Context) {
	if err := p.pipes.Reload(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(p.pipes.List())})
}

// produceRequest 出片请求。
type produceRequest struct {
	Pipeline  string                 `json:"pipeline"`
	Params    map[string]interface{} `json:"params"`
	InvokedBy string                 `json:"invoked_by"`
	// QCRounds 覆盖默认轮数（可选）
	QCRounds int `json:"qc_rounds,omitempty"`
}

// Produce 起一次出片。
func (p *Producer) Produce(c *gin.Context) {
	var req produceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {pipeline, params}"})
		return
	}
	run, status, errMsg := p.start(req)
	if errMsg != "" {
		ret := gin.H{"error": errMsg}
		if run != nil {
			ret["run_id"] = run.ID
		}
		c.JSON(status, ret)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"run_id":    run.ID,
		"pipeline":  run.PipelineID,
		"status":    run.Status,
		"params":    json.RawMessage(run.Params),
		"created_at": run.CreatedAt,
	})
}

// start 是 Produce / Rerun / MCP 共用的真正起跑逻辑。
func (p *Producer) start(req produceRequest) (*model.WorkflowRun, int, string) {
	built, err := p.pipes.Build(req.Pipeline, req.Params)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}

	nodes, edges := built.Nodes, built.Edges

	// 保存期参数校验 + 依赖预检，都针对**用户真正要的那张图**，
	// 必须在质量循环包壳之前做：壳里只有一个 quality_loop 节点，
	// 对壳做预检等于什么都没查（那会变成"最贵的路径反而跳过预检"）。
	if issues := service.ValidateNodeParams(nodes, service.InboundSet(edges)); len(issues) > 0 {
		msgs := make([]string, 0, len(issues))
		for _, it := range issues {
			msgs = append(msgs, it.Message)
		}
		return nil, http.StatusBadRequest, strings.Join(msgs, "；")
	}
	if missing := p.preflight(nodes, edges); len(missing) > 0 {
		return nil, http.StatusServiceUnavailable, preflightText(missing)
	}

	// 质量循环：把「刚组好的这张图」落成一条临时工作流，再用 quality_loop 包起来。
	// 为什么不直接内嵌：quality_loop 的语义就是"反复重跑另一张图并择优"，
	// 它按 workflow_id 从库里取图（engine.SetResolver）。
	var transientID uint
	if qc, _ := built.Values["qc"].(bool); qc {
		outer, id, werr := p.wrapQualityLoop(built, req.QCRounds)
		if werr != nil {
			return nil, http.StatusBadRequest, werr.Error()
		}
		nodes, edges, transientID = outer.Nodes, outer.Edges, id
	}

	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "节点图序列化失败：" + err.Error()
	}
	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "连线序列化失败：" + err.Error()
	}

	paramsJSON, _ := json.Marshal(built.Values)
	now := time.Now()
	run := &model.WorkflowRun{
		WorkflowID: transientID,
		PipelineID: built.Pipeline.ID,
		Params:     json.RawMessage(paramsJSON),
		Status:     "running",
		InvokedBy:  normalizeInvoker(req.InvokedBy),
		StartedAt:  &now,
		CreatedAt:  now,
	}
	if err := p.db.Create(run).Error; err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "运行记录写入失败：" + err.Error()
	}

	job := model.Job{
		RunID:      run.ID,
		WorkflowID: transientID,
		Status:     "pending",
		Trigger:    run.InvokedBy,
		CreatedAt:  now,
	}
	p.db.Create(&job)

	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		p.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	exec := func() {
		nodeStates, execErr := p.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		job.EndedAt = &endNow
		switch {
		case execErr != nil && service.ErrorCodeOf(execErr) == model.ErrorCodeCancelled:
			run.Status = "cancelled"
			run.Error = execErr.Error()
			job.Status = "interrupted"
			job.Error = execErr.Error()
		case execErr != nil:
			run.Status = "failed"
			run.Error = execErr.Error()
			job.Status = "failed"
			job.Error = execErr.Error()
		default:
			run.Status = "success"
			job.Status = "done"
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		p.db.Save(run)
		p.db.Save(&job)
		p.dropTransient(transientID)
	}

	if p.queue != nil {
		job.Status = "running"
		job.StartedAt = &now
		p.db.Save(&job)
		p.queue.Enqueue(job, exec)
	} else {
		go exec()
	}

	return run, http.StatusAccepted, ""
}

// wrapQualityLoop 把组装好的图收进一条临时工作流，外层只留 start → quality_loop → end。
func (p *Producer) wrapQualityLoop(built *service.BuiltPipeline, rounds int) (*service.BuiltPipeline, uint, error) {
	if rounds <= 0 || rounds > 8 {
		rounds = p.qcRounds
	}
	nodesJSON, err := json.Marshal(built.Nodes)
	if err != nil {
		return nil, 0, err
	}
	edgesJSON, err := json.Marshal(built.Edges)
	if err != nil {
		return nil, 0, err
	}
	p.mu.Lock()
	inner := model.Workflow{Name: "qc:" + built.Pipeline.ID, Nodes: nodesJSON, Edges: edgesJSON}
	err = p.db.Create(&inner).Error
	p.mu.Unlock()
	if err != nil {
		return nil, 0, fmt.Errorf("质量循环临时图写入失败：%w", err)
	}

	prompt, _ := built.Values["prompt"].(string)
	outer := &service.BuiltPipeline{
		Pipeline: built.Pipeline,
		Values:   built.Values,
		Nodes: []model.NodeInstance{
			{ID: "start-1", Type: "start", Data: map[string]interface{}{"prompt": prompt}, Status: "pending"},
			{ID: "ql-1", Type: "quality_loop", Status: "pending", Data: map[string]interface{}{
				"workflow_id":  float64(inner.ID),
				"max_rounds":   float64(rounds),
				"target_score": float64(100),
				"repair":       "auto",
				"input":        prompt,
			}},
			{ID: "end-1", Type: "end", Data: map[string]interface{}{}, Status: "pending"},
		},
		Edges: []model.EdgeInstance{
			{ID: "e1", Source: "start-1", Target: "ql-1"},
			{ID: "e2", Source: "ql-1", Target: "end-1"},
		},
	}
	return outer, inner.ID, nil
}

func (p *Producer) dropTransient(id uint) {
	if id == 0 {
		return
	}
	p.db.Delete(&model.Workflow{}, id)
}

// preflight 现场探测依赖并核对节点需求。
func (p *Producer) preflight(nodes []model.NodeInstance, edges []model.EdgeInstance) []service.DepRequirement {
	if p.sup == nil {
		return nil
	}
	caps := service.ProbeCapabilities(nil, "", p.ollamaBase, p.comfyBase, p.comfyH3Base, p.asrBase, p.ttsBase)
	return service.PreflightDeps(nodes, caps, p.sup)
}

// preflightText 把缺失清单压成一句话，每项带修复动作。
func preflightText(missing []service.DepRequirement) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		item := m.Label
		if m.Fix != "" {
			item += " → " + m.Fix
		}
		parts = append(parts, item)
	}
	return "依赖服务未就绪，未开始出片：" + strings.Join(parts, "、")
}

// normalizeInvoker 只接受已知调用方；未知一律记 manual（不猜）。
func normalizeInvoker(v string) string {
	switch v {
	case "workbuddy", "dsh", "mcp", "manual":
		return v
	case "":
		return "manual"
	}
	return "manual"
}

// PreflightPipelines 出片前体检：按流水线+字段组装一次图，只探测依赖不运行。
func (p *Producer) PreflightPipelines(c *gin.Context) {
	var req produceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {pipeline, params}"})
		return
	}
	built, err := p.pipes.Build(req.Pipeline, req.Params)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	missing := p.preflight(built.Nodes, built.Edges)
	if missing == nil {
		// nil 切片会被序列化成 null，前端/智能体拿到 null 会当"字段不存在"。
		// 「什么都不缺」必须是空数组，不能是 null。
		missing = []service.DepRequirement{}
	}
	fixable := 0
	for _, m := range missing {
		if m.Fixable {
			fixable++
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": len(missing) == 0, "missing": missing, "fixable": fixable})
}

// GetRun 单次出片详情：状态 + 节点终态 + 生效参数。
func (p *Producer) GetRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	var run model.WorkflowRun
	if err := p.db.First(&run, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	var params interface{}
	if len(run.Params) > 0 {
		_ = json.Unmarshal(run.Params, &params)
	}
	c.JSON(http.StatusOK, gin.H{
		"run_id":      run.ID,
		"pipeline":    run.PipelineID,
		"status":      run.Status,
		"error":       run.Error,
		"invoked_by":  run.InvokedBy,
		"params":      params,
		"node_states": json.RawMessage(run.NodeStates),
		"started_at":  run.StartedAt,
		"ended_at":    run.EndedAt,
		"created_at":  run.CreatedAt,
	})
}

// CancelRun 中止进行中的出片。
func (p *Producer) CancelRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	if !p.engine.CancelRun(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "该运行已结束或未在进行中"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": id, "status": "cancelling"})
}

// Rerun 用同一份参数再跑一次（换了字段就用 produce）。
func (p *Producer) Rerun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	var old model.WorkflowRun
	if err := p.db.First(&old, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	var params map[string]interface{}
	if len(old.Params) > 0 {
		if err := json.Unmarshal(old.Params, &params); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "这条运行没有可复用的参数"})
			return
		}
	}
	run, status, msg := p.start(produceRequest{
		Pipeline:  old.PipelineID,
		Params:    params,
		InvokedBy: old.InvokedBy,
	})
	if msg != "" {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"run_id": run.ID, "pipeline": run.PipelineID, "status": run.Status})
}
