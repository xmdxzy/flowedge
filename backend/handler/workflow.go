package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

type WorkflowHandler struct {
	db      *gorm.DB
	engine  *service.EngineV2
	queue   *service.JobQueue      // nil 时退化为直接 goroutine 起跑（保持向后兼容）
	lineage *service.LineageStore  // nil 时跳过血缘记录
}

func NewWorkflowHandler(db *gorm.DB, engine *service.EngineV2, queue *service.JobQueue) *WorkflowHandler {
	return &WorkflowHandler{db: db, engine: engine, queue: queue}
}

// SetLineage 注入血缘仓库（运行结束后记录产物来源）
func (h *WorkflowHandler) SetLineage(l *service.LineageStore) {
	h.lineage = l
}

// List 获取所有工作流
func (h *WorkflowHandler) List(c *gin.Context) {
	var workflows []model.Workflow
	h.db.Order("updated_at DESC").Find(&workflows)
	c.JSON(http.StatusOK, workflows)
}

// Get 获取单个工作流
func (h *WorkflowHandler) Get(c *gin.Context) {
	id := c.Param("id")
	var workflow model.Workflow
	if err := h.db.First(&workflow, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	c.JSON(http.StatusOK, workflow)
}

// validateGraph 保存期图结构校验：阻止把非法 DAG 存进库。
// 返回 (0, nil) 表示通过；否则返回 HTTP 状态码与错误体。
// 只阻断 error 级问题（环 / 自环 / 悬空边），warn 级（重复边、孤立节点）随响应一并返回供前端提示。
func validateGraph(nodes, edges json.RawMessage) (int, gin.H) {
	issues, err := service.ValidateDAGJSON(nodes, edges)
	if err != nil {
		return http.StatusBadRequest, gin.H{"error": err.Error()}
	}
	blocking := make([]service.DAGIssue, 0)
	for _, i := range issues {
		if i.Level == "error" {
			blocking = append(blocking, i)
		}
	}
	if len(blocking) > 0 {
		return http.StatusBadRequest, gin.H{
			"error":  blocking[0].Message,
			"code":   string(blocking[0].Code),
			"issues": issues,
		}
	}
	return 0, nil
}

// Create 创建工作流
func (h *WorkflowHandler) Create(c *gin.Context) {
	var wf model.Workflow
	if err := c.ShouldBindJSON(&wf); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if code, body := validateGraph(wf.Nodes, wf.Edges); code != 0 {
		c.JSON(code, body)
		return
	}
	if err := h.db.Create(&wf).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// Update 更新工作流（局部更新）
//
// 之前是无条件写 name/nodes/edges 三个字段：调用方只想改名时，nodes/edges 为空
// 就会把整张图覆盖成 null——一次「重命名」直接把工作流清空，属于静默数据丢失。
// 现在只更新请求体里真正出现的字段。
func (h *WorkflowHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var wf model.Workflow
	if err := h.db.First(&wf, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	var updates struct {
		Name  *string         `json:"name"`
		Nodes json.RawMessage `json:"nodes"`
		Edges json.RawMessage `json:"edges"`
	}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fields := map[string]interface{}{}
	if updates.Name != nil && strings.TrimSpace(*updates.Name) != "" {
		fields["name"] = strings.TrimSpace(*updates.Name)
	}
	if len(updates.Nodes) > 0 || len(updates.Edges) > 0 {
		// 图结构必须成对提交：只改节点不改连线会得到一张自相矛盾的图
		if len(updates.Nodes) == 0 || len(updates.Edges) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nodes 与 edges 必须一起提交"})
			return
		}
		if code, body := validateGraph(updates.Nodes, updates.Edges); code != 0 {
			c.JSON(code, body)
			return
		}
		fields["nodes"] = updates.Nodes
		fields["edges"] = updates.Edges
	}
	if len(fields) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	h.db.Model(&wf).Updates(fields)
	h.db.First(&wf, id)
	c.JSON(http.StatusOK, wf)
}

// Delete 删除工作流
func (h *WorkflowHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	h.db.Delete(&model.Workflow{}, id)
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// Run 执行工作流
// StartRun 创建执行记录并异步跑完（触发器与手动运行共用同一条路径）。
// trigger 可选：manual / webhook / cron；留空按 manual 记录。
func (h *WorkflowHandler) StartRun(wf *model.Workflow, trigger ...string) *model.WorkflowRun {
	nodesJSON := []byte(wf.Nodes)
	edgesJSON := []byte(wf.Edges)

	now := time.Now()
	run := model.WorkflowRun{
		WorkflowID: wf.ID,
		Status:     "running",
		StartedAt:  &now,
	}
	h.db.Create(&run)

	// 任务落库：进程崩溃后 DB 里仍保留「曾受理过」的事实，
	// 下次启动由 JobQueue.RecoverInterrupted 把残留 running 标记为 interrupted
	jobTrigger := "manual"
	if len(trigger) > 0 && trigger[0] != "" {
		jobTrigger = trigger[0]
	}
	job := model.Job{
		RunID:      run.ID,
		WorkflowID: wf.ID,
		Status:     "pending",
		Trigger:    jobTrigger,
		CreatedAt:  now,
	}
	h.db.Create(&job)

	// 增量落库：每层完成后更新 node_states（断点续跑依据）
	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		h.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	// 断点：节点上勾了 breakpoint 就停，运行时不必再开调试模式
	breakpoints := collectBreakpoints(nodesJSON)
	var rc *service.RunControl
	if len(breakpoints) > 0 {
		rc = service.NewBreakpointControl(mapFromSlice(breakpoints))
	}

	exec := func() {
		var nodeStates []model.NodeInstance
		var err error
		if rc != nil {
			// 走可取消 + 断点控制入口
			nodeStates, err = h.engine.ExecuteWithControl(nodesJSON, edgesJSON, run.ID, rc, persist)
		} else {
			nodeStates, err = h.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)
		}

		paused, pauseAt, pauseReason := "", "", ""
		if rc != nil {
			p, at, reason := rc.State()
			if p {
				paused, pauseAt, pauseReason = "paused", at, reason
			}
		}

		endNow := time.Now()
		run.EndedAt = &endNow
		job.EndedAt = &endNow
		switch {
		case err != nil && service.ErrorCodeOf(err) == model.ErrorCodeCancelled:
			// 主动中止 ≠ 失败：台账与界面都要能区分「我按停的」和「跑挂了」
			run.Status = "cancelled"
			run.Error = err.Error()
			job.Status = "interrupted"
			job.Error = err.Error()
		case err != nil:
			run.Status = "failed"
			run.Error = err.Error()
			job.Status = "failed"
			job.Error = err.Error()
		case paused != "":
			run.Status = "paused"
			run.PausedAt = pauseAt
			run.PauseReason = pauseReason
			job.Status = "paused"
		default:
			run.Status = "success"
			job.Status = "done"
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(&run)
		h.db.Save(&job)

		// 产物血缘：运行结束（含暂停）都记一次，已产出的图/音/视频都能溯源
		if h.lineage != nil {
			var edges []model.EdgeInstance
			_ = json.Unmarshal(edgesJSON, &edges)
			h.lineage.RecordRun(wf.ID, run.ID, nodeStates, edges)
		}
	}

	if h.queue != nil {
		job.Status = "running"
		job.StartedAt = &now
		h.db.Save(&job)
		h.queue.Enqueue(job, exec)
	} else {
		go exec()
	}

	return &run
}

// CancelRun 中止进行中的运行（已完成的节点保留，剩余标记为 skipped）
func (h *WorkflowHandler) CancelRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	if !h.engine.CancelRun(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "该运行已结束或未在进行中"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": id, "status": "cancelling"})
}

// Run 手动执行工作流
func (h *WorkflowHandler) Run(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	run := h.StartRun(&wf)
	c.JSON(http.StatusAccepted, gin.H{"run_id": run.ID, "status": "running"})
}

// ResumeRun 断点续跑：复用已完成节点输出，只重跑失败 / 未执行节点
func (h *WorkflowHandler) ResumeRun(c *gin.Context) {
	h.resumeFrom(c, "")
}

// ResumeFromRun 从指定节点开始重跑（单节点重跑 / 「从这继续」）
// body: {"node_id": "b"}
func (h *WorkflowHandler) ResumeFromRun(c *gin.Context) {
	var body struct {
		NodeID string `json:"node_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.NodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing node_id"})
		return
	}
	h.resumeFrom(c, body.NodeID)
}

// resumeFrom 断点续跑公共实现；fromNode 为空时等同整体续跑
func (h *WorkflowHandler) resumeFrom(c *gin.Context, fromNode string) {
	runID := c.Param("runId")
	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if run.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "run is still running"})
		return
	}

	var wf model.Workflow
	if err := h.db.First(&wf, run.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	prev := []model.NodeInstance{}
	if len(run.NodeStates) > 0 {
		if err := json.Unmarshal(run.NodeStates, &prev); err != nil {
			prev = []model.NodeInstance{}
		}
	}

	// fromNode 必须存在于本次运行的节点里，否则 400（避免拼写错误的节点 id 被静默忽略）
	if fromNode != "" {
		found := false
		for _, n := range prev {
			if n.ID == fromNode {
				found = true
				break
			}
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{"error": "node not found in this run", "node_id": fromNode})
			return
		}
	}

	now := time.Now()
	run.Status = "running"
	run.Error = ""
	run.EndedAt = nil
	run.StartedAt = &now
	h.db.Save(&run)

	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		h.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	job := model.Job{
		RunID:      run.ID,
		WorkflowID: run.WorkflowID,
		Status:     "running",
		Trigger:    "resume",
		CreatedAt:  time.Now(),
		StartedAt:  run.StartedAt,
	}
	h.db.Create(&job)

	exec := func() {
		// 走带 runID 的可取消入口：续跑的事件也要归属性，否则执行面板看不到重跑过程
		nodeStates, err := h.engine.ResumeFromWithRunID([]byte(wf.Nodes), []byte(wf.Edges), prev, fromNode, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow

		if err != nil {
			if service.ErrorCodeOf(err) == model.ErrorCodeCancelled {
				run.Status = "cancelled"
			} else {
				run.Status = "failed"
			}
			run.Error = err.Error()
		} else {
			run.Status = "success"
		}

		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(&run)

		job := model.Job{
			RunID: run.ID, WorkflowID: run.WorkflowID,
			Status: "done", Trigger: "resume", CreatedAt: *run.StartedAt,
			StartedAt: run.StartedAt, EndedAt: &endNow,
		}
		if err != nil {
			if service.ErrorCodeOf(err) == model.ErrorCodeCancelled {
				job.Status = "interrupted"
			} else {
				job.Status = "failed"
			}
			job.Error = err.Error()
		}
		h.db.Save(&job)
	}

	if h.queue != nil {
		h.queue.Enqueue(job, exec)
	} else {
		go exec()
	}

	c.JSON(http.StatusAccepted, gin.H{
		"run_id":     run.ID,
		"status":     "running",
		"resumed":    true,
		"prev_nodes": len(prev),
	})
}

// GetRun 获取执行记录
func (h *WorkflowHandler) GetRun(c *gin.Context) {
	runID := c.Param("runId")
	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	c.JSON(http.StatusOK, run)
}

// ListRuns 列出某工作流的执行记录。
// 保持返回数组以兼容前端执行面板；分页与状态过滤见 ?limit=&offset=&status=（默认 50，上限 200）
func (h *WorkflowHandler) ListRuns(c *gin.Context) {
	wfID := c.Param("id")
	limit := parseIntDefault(c.Query("limit"), 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	status := c.Query("status")

	q := h.db.Model(&model.WorkflowRun{}).Where("workflow_id = ?", wfID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var runs []model.WorkflowRun
	q.Order("created_at DESC").Limit(limit).Find(&runs)

	c.JSON(http.StatusOK, runs)
}
