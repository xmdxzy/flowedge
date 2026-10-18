package handler

import (
	"encoding/json"
	"net/http"
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

// Create 创建工作流
func (h *WorkflowHandler) Create(c *gin.Context) {
	var wf model.Workflow
	if err := c.ShouldBindJSON(&wf); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
func (h *WorkflowHandler) Run(c *gin.Context) {
	id := c.Param("id")
	var wf model.Workflow
	if err := h.db.First(&wf, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	nodesJSON := []byte(wf.Nodes)
	edgesJSON := []byte(wf.Edges)

	// 创建执行记录
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

	// 异步执行
	go func() {
		nodeStates, err := h.engine.ExecutePersistent(nodesJSON, edgesJSON, persist)

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
		"run_id": run.ID,
		"status": "running",
	})
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
