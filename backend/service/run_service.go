package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 续跑编排（原 handler/workflow.go 里的 123 行）。
//
// 为什么下沉：那一段是**应用逻辑**——校验运行状态、挑节点图、落 job、跑完回写状态，
// 与 HTTP 无关。放在 handler 里导致三件事做不到：
//   1. 无法脱离 gin 测试（只能起服务或手搓 httptest 上下文）；
//   2. 单镜重跑（handler/shots.go）复用它时，只能再传一个 gin.Context 进去；
//   3. 同一套状态机在「运行 / 续跑 / 单镜重跑」三处各写一遍，迟早写歪。
// handler 现在只负责把 error 映射成 HTTP 状态码。

var (
	ErrRunNotFound      = errors.New("run not found")
	ErrWorkflowNotFound = errors.New("workflow not found")
	ErrRunStillRunning  = errors.New("run is still running")
)

// NodeNotFoundError fromNode 不在本次运行的节点里（多半是拼错了 id）。
// 必须显式失败：静默忽略会让人以为"重跑了"，其实什么都没发生。
type NodeNotFoundError struct{ NodeID string }

func (e *NodeNotFoundError) Error() string {
	return fmt.Sprintf("node not found in this run: %s", e.NodeID)
}

// ResumeOutcome 续跑已被受理（异步执行，不等结果）
type ResumeOutcome struct {
	RunID     uint
	PrevNodes int
}

// RunService 运行的续跑/重跑编排
type RunService struct {
	db     *gorm.DB
	engine *EngineV2
	queue  *JobQueue

	// runAsync 执行调度：默认走队列（满则阻塞）或裸 goroutine。
	// 抽成字段是为了让测试能同步执行——否则只能 sleep 等异步结果。
	runAsync func(job model.Job, fn func())
}

func NewRunService(db *gorm.DB, engine *EngineV2, queue *JobQueue) *RunService {
	s := &RunService{db: db, engine: engine, queue: queue}
	s.runAsync = func(job model.Job, fn func()) {
		if queue != nil {
			queue.Enqueue(job, fn)
			return
		}
		go fn()
	}
	return s
}

// SetSynchronous 测试用：把任务改成同步执行，返回时状态已落库。
func (s *RunService) SetSynchronous() {
	s.runAsync = func(_ model.Job, fn func()) { fn() }
}

// Resume 断点续跑：复用已完成节点输出，只重跑失败/未执行节点。
//
// fromNode 为空 = 整体续跑；非空 = 从该节点开始（单节点重跑 / 单镜重跑）。
// nodesOverride 非空时用改过的图跑，但**不写回数据库**——单镜重跑只改这一次调用，
// 否则用户保存的工作流会被一次临时操作永久改掉。
func (s *RunService) Resume(runID uint, fromNode string, nodesOverride []byte) (ResumeOutcome, error) {
	out := ResumeOutcome{RunID: runID}

	var run model.WorkflowRun
	// 用 Find 而不是 First：First 找不到会打一条 "record not found" 噪音日志，
	// 而「续跑一个已删除的运行」是正常路径（用户在历史面板点了旧记录）。
	if res := s.db.Limit(1).Find(&run, runID); res.Error != nil || run.ID == 0 {
		return out, ErrRunNotFound
	}
	if run.Status == "running" {
		return out, ErrRunStillRunning
	}

	var wf model.Workflow
	if err := s.db.First(&wf, run.WorkflowID).Error; err != nil {
		return out, ErrWorkflowNotFound
	}

	prev := []model.NodeInstance{}
	if len(run.NodeStates) > 0 {
		if err := json.Unmarshal(run.NodeStates, &prev); err != nil {
			prev = []model.NodeInstance{}
		}
	}
	out.PrevNodes = len(prev)

	if fromNode != "" {
		found := false
		for _, n := range prev {
			if n.ID == fromNode {
				found = true
				break
			}
		}
		if !found {
			return out, &NodeNotFoundError{NodeID: fromNode}
		}
	}

	now := time.Now()
	run.Status = "running"
	run.Error = ""
	run.EndedAt = nil
	run.StartedAt = &now
	if err := s.db.Save(&run).Error; err != nil {
		return out, fmt.Errorf("更新运行状态失败：%w", err)
	}

	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		s.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
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
	s.db.Create(&job)

	nodesJSON := []byte(wf.Nodes)
	if len(nodesOverride) > 0 {
		nodesJSON = nodesOverride
	}

	s.runAsync(job, func() {
		// 走带 runID 的可取消入口：续跑的事件也要归属性，否则执行面板看不到重跑过程
		nodeStates, err := s.engine.ResumeFromWithRunID(
			nodesJSON, []byte(wf.Edges), prev, fromNode, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		if err != nil {
			if ErrorCodeOf(err) == model.ErrorCodeCancelled {
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
		s.db.Save(&run)

		done := model.Job{
			RunID: run.ID, WorkflowID: run.WorkflowID,
			Status: "done", Trigger: "resume", CreatedAt: *run.StartedAt,
			StartedAt: run.StartedAt, EndedAt: &endNow,
		}
		if err != nil {
			if ErrorCodeOf(err) == model.ErrorCodeCancelled {
				done.Status = "interrupted"
			} else {
				done.Status = "failed"
			}
			done.Error = err.Error()
		}
		s.db.Save(&done)
	})

	return out, nil
}
