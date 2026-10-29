package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 画布图的运行入口。
//
// 为什么单独一个文件而不是塞进 produce.go：Produce 的语义是「选一条流水线 + 填字段」，
// 它的开头就是 `pipes.Build(id, params)`；画布的图**已经不是流水线了**，
// 硬塞进去会变成一堆 `if custom {...}` 分支，而那条主路径已经够长了。
//
// 复用而不是重写：预检用 p.preflight、显存用 p.vramBlockNow、排队用
// queue.EnqueueClass、执行用 engine.ExecuteWithRunID —— 与流水线出片**同一条链路**，
// 否则画布出来的片子会绕开门控、队列与取消语义（那就是第二套系统了）。
//
// 四条边界：
//  1. 运行前**再校验一次**（结构/端口/参数）。保存时的校验挡不住"保存后被手改的库记录"，
//     而运行是唯一真正花钱的动作，值得再查一遍。
//  2. `PipelineID` 记成 `graph:<id>`（质检时 `graph:<id>:qc`），运行页据此显示来源，
//     重跑也走回同一张图。
//  3. 质量循环由请求参数开（qc=true），与流水线出片同一套包壳语义：内层图落成
//     `qc:` 临时工作流，外层换 start → quality_loop → end，跑完即删、启动时清扫。
//  4. 画布图**不做**"另开一套画布运行时"：节点执行体只有 engine_v2_nodes.go 一处。

const graphPipelinePrefix = "graph:"

type runGraphRequest struct {
	InvokedBy        string `json:"invoked_by,omitempty"`
	AllowPartialVRAM bool   `json:"allow_partial_vram,omitempty"`
	// QC 让「画布图也能开质量循环」，不必自己先建两张图再手工接 quality_loop。
	QC       bool `json:"qc,omitempty"`
	QCRounds int  `json:"qc_rounds,omitempty"`
}

// RunWorkflow 运行一张已保存的图：POST /api/workflows/:id/run
func (p *Producer) RunWorkflow(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "工作流 id 必须是正整数"})
		return
	}
	var req runGraphRequest
	// 允许空 body：只想点一下运行，不想构造 payload。
	_ = c.ShouldBindJSON(&req)

	run, status, msg := p.runGraph(uint(id), req.InvokedBy, req.AllowPartialVRAM, req.QC, req.QCRounds)
	if msg != "" {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"run_id":      run.ID,
		"workflow_id": run.WorkflowID,
		"pipeline":    run.PipelineID,
		"status":      run.Status,
	})
}

// runGraph 把一张库里的图跑起来。
func (p *Producer) runGraph(wfID uint, invokedBy string, allowPartial, qc bool, qcRounds int) (*model.WorkflowRun, int, string) {
	var wf model.Workflow
	// 用 Limit(1).Find：First 找不到会打 "record not found" 噪音日志
	if res := p.db.Limit(1).Find(&wf, wfID); res.Error != nil || wf.ID == 0 {
		return nil, http.StatusNotFound, fmt.Sprintf("没有 id=%d 的工作流", wfID)
	}
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		return nil, http.StatusBadRequest, "库里的 nodes 不是合法 JSON：" + err.Error()
	}
	if err := json.Unmarshal(wf.Edges, &edges); err != nil {
		return nil, http.StatusBadRequest, "库里的 edges 不是合法 JSON：" + err.Error()
	}
	if len(nodes) == 0 {
		return nil, http.StatusBadRequest, "图里一个节点都没有"
	}

	// ① 三层校验：结构 / 端口类型 / 参数。运行是唯一花钱的动作，必须再查一次。
	if rep := service.ValidateGraph(nodes, edges); !rep.OK {
		detail := firstBlockingMessage(rep)
		if detail == "" {
			detail = "存在阻断问题"
		}
		return nil, http.StatusBadRequest, fmt.Sprintf(
			"图有问题，不能运行（%s）：%s", strings.Join(rep.BlockingCodes, ", "), detail)
	}

	// ② 依赖预检：缺服务在入口就说清，别等节点跑进去才报连不上
	if missing := p.preflight(nodes, edges); len(missing) > 0 {
		return nil, http.StatusServiceUnavailable, preflightText(missing)
	}

	// ③ 显存前瞻（外来占用才拦；自家队列在跑/排队时放行并排到后面，理由见 produce.go）
	if msg := p.vramBlockNow(nodes, allowPartial); msg != "" {
		return nil, http.StatusConflict, msg
	}

	// ④ 需要质检就把内层图单独落一份，外层只留三个节点。
	// 关键：预检与参数校验已经在**内层**做完（对壳做等于什么都没查）。
	if qc {
		nodesJSON, edgesJSON, transientID, err := p.wrapGraphQC(wf, nodes, qcRounds)
		if err != nil {
			return nil, http.StatusInternalServerError, "质量循环包壳失败：" + err.Error()
		}
		run, status, msg := p.startGraphRun(wf, nodesJSON, edgesJSON, invokedBy, allowPartial, true)
		if msg != "" && transientID != 0 {
			p.dropTransient(transientID)
		}
		return run, status, msg
	}
	return p.startGraphRun(wf, wf.Nodes, wf.Edges, invokedBy, allowPartial, false)
}

// wrapGraphQC 把「用户那张图」落成 qc: 临时工作流，并返回外层壳的 nodes/edges。
//
// 为什么必须先落库：quality_loop 的语义是"反复重跑另一张图并择优"，它按
// workflow_id 从库里取图（engine.SetResolver）。不落库就没法循环。
func (p *Producer) wrapGraphQC(wf model.Workflow, innerNodes []model.NodeInstance, rounds int) ([]byte, []byte, uint, error) {
	if rounds <= 0 || rounds > 8 {
		rounds = 3
	}
	transient := model.Workflow{
		Name:          fmt.Sprintf("qc:graph-%d", wf.ID),
		Nodes:         wf.Nodes,
		Edges:         wf.Edges,
		SchemaVersion: wf.SchemaVersion,
	}
	if err := p.db.Create(&transient).Error; err != nil {
		return nil, nil, 0, err
	}

	// 外层 start 的提示词沿用内层入口节点的提示词：quality_loop 把它当 seedText
	// 传进每一轮；壳里没有提示词就会每轮重跑一张没有主题的图。
	seed := ""
	for _, n := range innerNodes {
		if n.Type != "start" {
			continue
		}
		if s, ok := n.Data["prompt"].(string); ok && strings.TrimSpace(s) != "" {
			seed = s
			break
		}
	}
	if seed == "" {
		seed = wf.Name
	}

	outer := []model.NodeInstance{
		{ID: "qc-start", Type: "start", Data: map[string]interface{}{"prompt": seed}, Status: "pending"},
		{ID: "qc-loop", Type: "quality_loop", Status: "pending", Data: map[string]interface{}{
			"workflow_id": transient.ID, "max_rounds": rounds, "target_score": 85,
		}},
		{ID: "qc-end", Type: "end", Status: "pending"},
	}
	outerEdges := []model.EdgeInstance{
		{ID: "qc-e1", Source: "qc-start", Target: "qc-loop"},
		{ID: "qc-e2", Source: "qc-loop", Target: "qc-end"},
	}
	nodesJSON, err := json.Marshal(outer)
	if err != nil {
		p.dropTransient(transient.ID)
		return nil, nil, 0, err
	}
	edgesJSON, err := json.Marshal(outerEdges)
	if err != nil {
		p.dropTransient(transient.ID)
		return nil, nil, 0, err
	}
	return nodesJSON, edgesJSON, transient.ID, nil
}

// startGraphRun 建 run + job，然后交给队列执行。与 Produce 的 start() 尾部同构。
func (p *Producer) startGraphRun(wf model.Workflow, nodesJSON, edgesJSON []byte, invokedBy string, allowPartial, qc bool) (*model.WorkflowRun, int, string) {
	var nodes []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nil, http.StatusInternalServerError, "节点图序列化失败：" + err.Error()
	}

	pipelineID := fmt.Sprintf("%s%d", graphPipelinePrefix, wf.ID)
	if qc {
		pipelineID += ":qc"
	}

	now := time.Now()
	run := &model.WorkflowRun{
		WorkflowID: wf.ID,
		PipelineID: pipelineID,
		Status:     "running",
		InvokedBy:  normalizeInvoker(invokedBy),
		StartedAt:  &now,
		CreatedAt:  now,
	}
	if err := p.db.Create(run).Error; err != nil {
		return nil, http.StatusInternalServerError, "运行记录写入失败：" + err.Error()
	}

	job := model.Job{
		RunID:      run.ID,
		WorkflowID: wf.ID,
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
		if msg := p.vramBlockNow(nodes, allowPartial); msg != "" {
			endNow := time.Now()
			run.EndedAt, run.Status, run.Error = &endNow, "failed", msg
			job.Status, job.Error, job.EndedAt = "failed", msg, &endNow
			p.db.Save(run)
			p.db.Model(&model.Job{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
				"status": job.Status, "error": job.Error, "ended_at": endNow,
			})
			log.Printf("[graph-run] run=%d 开跑前复判被拦下：%s", run.ID, msg)
			return
		}

		nodeStates, execErr := p.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		job.EndedAt = &endNow
		switch {
		case execErr != nil && service.ErrorCodeOf(execErr) == model.ErrorCodeCancelled:
			run.Status, run.Error = "cancelled", execErr.Error()
			job.Status, job.Error = "interrupted", execErr.Error()
		case execErr != nil:
			run.Status, run.Error = "failed", execErr.Error()
			job.Status, job.Error = "failed", execErr.Error()
		default:
			run.Status, job.Status = "success", "done"
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		p.db.Save(run)
		p.db.Model(&model.Job{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
			"status": job.Status, "error": job.Error, "ended_at": endNow,
		})
	}

	if p.queue != nil {
		p.queue.EnqueueClass(job, service.JobClassForNodes(nodes), exec)
	} else {
		job.Status = "running"
		job.StartedAt = &now
		p.db.Save(&job)
		go exec()
	}
	return run, http.StatusAccepted, ""
}

// IsGraphRun 这次运行是不是画布图（重跑与看板据此分流）。
func IsGraphRun(pipelineID string) bool {
	return strings.HasPrefix(pipelineID, graphPipelinePrefix)
}

// GraphWorkflowID 从 graph:<id> / graph:<id>:qc 里取回图 id。
func GraphWorkflowID(pipelineID string) (uint, bool) {
	if !IsGraphRun(pipelineID) {
		return 0, false
	}
	raw := strings.TrimPrefix(pipelineID, graphPipelinePrefix)
	if i := strings.Index(raw, ":"); i >= 0 {
		raw = raw[:i] // 去掉 :qc 后缀
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}
