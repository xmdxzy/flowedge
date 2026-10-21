package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// DebugHandler 单步调试与断点续跑
//
// 协议：
//
//	POST /api/runs/:runId/step      执行「下一个节点」后停在它之后
//	POST /api/runs/:runId/continue  一直跑到结束，或撞上下一个断点
//
// 两者都是异步的（节点可能跑几分钟，不能被 HTTP 超时打断），
// 前端靠轮询 GET /api/runs/:runId 拿到 paused_at / status=paused。
type DebugHandler struct {
	db      *gorm.DB
	engine  *service.EngineV2
	lineage *service.LineageStore

	mu   sync.Mutex
	busy map[uint]bool
}

func NewDebugHandler(db *gorm.DB, engine *service.EngineV2, lineage *service.LineageStore) *DebugHandler {
	return &DebugHandler{db: db, engine: engine, lineage: lineage, busy: map[uint]bool{}}
}

type debugRequest struct {
	Breakpoints []string `json:"breakpoints"`
	Budget      int      `json:"budget"`
}

// Step 单步：执行一个节点后暂停
func (h *DebugHandler) Step(c *gin.Context) {
	var req debugRequest
	_ = c.ShouldBindJSON(&req)
	budget := req.Budget
	if budget <= 0 {
		budget = 1
	}
	h.advance(c, true, budget, req.Breakpoints)
}

// Continue 继续：跑到下一个断点或结束
func (h *DebugHandler) Continue(c *gin.Context) {
	var req debugRequest
	_ = c.ShouldBindJSON(&req)
	h.advance(c, false, -1, req.Breakpoints)
}

func (h *DebugHandler) advance(c *gin.Context, step bool, budget int, breakpoints []string) {
	runID64, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	runID := uint(runID64)

	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if run.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "该运行正在执行，请先等待或用 /cancel 中止"})
		return
	}

	// 同一运行的调试推进必须串行：并发 step 会让两份 prev 状态互相覆盖
	h.mu.Lock()
	if h.busy[runID] {
		h.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "该运行正在推进中"})
		return
	}
	h.busy[runID] = true
	h.mu.Unlock()

	var wf model.Workflow
	if err := h.db.First(&wf, run.WorkflowID).Error; err != nil {
		h.release(runID)
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	prev := []model.NodeInstance{}
	if len(run.NodeStates) > 0 {
		_ = json.Unmarshal(run.NodeStates, &prev)
	}

	// 断点集合 = 请求体 ∪ 工作流里勾了断点的节点（前端两种入口都能用）
	bp := map[string]bool{}
	for _, id := range breakpoints {
		if id != "" {
			bp[id] = true
		}
	}
	for _, id := range collectBreakpoints([]byte(wf.Nodes)) {
		bp[id] = true
	}

	var rc *service.RunControl
	if step {
		rc = service.NewStepControl(budget)
	} else {
		rc = service.NewBreakpointControl(bp)
	}

	now := time.Now()
	run.Status = "running"
	run.Error = ""
	run.PausedAt = ""
	run.PauseReason = ""
	run.StartedAt = &now
	run.EndedAt = nil
	h.db.Save(&run)

	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		h.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	nodesJSON := []byte(wf.Nodes)
	edgesJSON := []byte(wf.Edges)

	go func() {
		defer h.release(runID)

		states, execErr := h.engine.ResumeWithControl(nodesJSON, edgesJSON, prev, run.ID, rc, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		paused, at, reason := rc.State()

		switch {
		case execErr != nil:
			run.Status = "failed"
			run.Error = execErr.Error()
		case paused:
			run.Status = "paused"
			run.PausedAt = at
			run.PauseReason = reason
		default:
			run.Status = "success"
		}
		statesJSON, _ := json.Marshal(states)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(&run)

		if h.lineage != nil {
			var edges []model.EdgeInstance
			_ = json.Unmarshal(edgesJSON, &edges)
			h.lineage.RecordRun(wf.ID, run.ID, states, edges)
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"run_id": runID,
		"status": "running",
		"mode":   map[bool]string{true: "step", false: "continue"}[step],
	})
}

func (h *DebugHandler) release(runID uint) {
	h.mu.Lock()
	delete(h.busy, runID)
	h.mu.Unlock()
}

// collectBreakpoints 从节点定义里取 data.breakpoint == true 的节点 ID
func collectBreakpoints(nodesJSON []byte) []string {
	var nodes []model.NodeInstance
	if json.Unmarshal(nodesJSON, &nodes) != nil {
		return nil
	}
	var out []string
	for _, n := range nodes {
		if v, ok := n.Data["breakpoint"].(bool); ok && v {
			out = append(out, n.ID)
		}
	}
	return out
}

// mapFromSlice 把节点 ID 列表转成集合
func mapFromSlice(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			m[id] = true
		}
	}
	return m
}
