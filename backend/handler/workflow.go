package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

type WorkflowHandler struct {
	db     *gorm.DB
	engine *service.EngineV2
}

func NewWorkflowHandler(db *gorm.DB, engine *service.EngineV2) *WorkflowHandler {
	return &WorkflowHandler{db: db, engine: engine}
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

// Update 更新工作流
func (h *WorkflowHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var wf model.Workflow
	if err := h.db.First(&wf, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	var updates model.Workflow
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if code, body := validateGraph(updates.Nodes, updates.Edges); code != 0 {
		c.JSON(code, body)
		return
	}

	h.db.Model(&wf).Updates(map[string]interface{}{
		"name":  updates.Name,
		"nodes": updates.Nodes,
		"edges": updates.Edges,
	})
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
// StartRun 创建执行记录并异步跑完（触发器与手动运行共用同一条路径）
func (h *WorkflowHandler) StartRun(wf *model.Workflow) *model.WorkflowRun {
	nodesJSON := []byte(wf.Nodes)
	edgesJSON := []byte(wf.Edges)

	now := time.Now()
	run := model.WorkflowRun{
		WorkflowID: wf.ID,
		Status:     "running",
		StartedAt:  &now,
	}
	h.db.Create(&run)

	// 增量落库：每层完成后更新 node_states（断点续跑依据）
	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		h.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	go func() {
		// 走可取消入口：前端可对进行中的运行发起中止
		nodeStates, err := h.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		if err != nil {
			run.Status = "failed"
			run.Error = err.Error()
		} else {
			run.Status = "success"
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(&run)
	}()

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

// ResumeRun 断点续跑：复用上次成功节点的输出，只重跑失败/未执行节点
func (h *WorkflowHandler) ResumeRun(c *gin.Context) {
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

	go func() {
		nodeStates, err := h.engine.Resume([]byte(wf.Nodes), []byte(wf.Edges), prev, persist)

		endNow := time.Now()
		run.EndedAt = &endNow

		if err != nil {
			run.Status = "failed"
			run.Error = err.Error()
		} else {
			run.Status = "success"
		}

		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(&run)
	}()

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

// ListRuns 列出执行记录
func (h *WorkflowHandler) ListRuns(c *gin.Context) {
	wfID := c.Param("id")
	var runs []model.WorkflowRun
	h.db.Where("workflow_id = ?", wfID).Order("created_at DESC").Limit(20).Find(&runs)
	c.JSON(http.StatusOK, runs)
}
