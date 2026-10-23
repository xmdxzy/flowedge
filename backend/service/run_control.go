package service

import (
	"context"
	"sync"
)

// RunControl 单步调试与断点控制。
//
// 设计要点：
//   - 引擎是「按层并行」的 DAG，真正的调试语义只能是「层内逐个执行」，
//     因此控制粒度落在**节点级执行许可**上：每个节点开始前向 RunControl 申请
//     一次许可（Take），没有许可就不执行、保持 pending。
//   - 断点用 ShouldPauseBefore：命中时该节点不执行、状态仍为 pending，
//     运行停在它之前——这正是用户按 F9 打断点时期待的画面。
//   - 控制对象由 HTTP 层逐次传入（每次 step/continue 新建一个），
//     天然规避「跨请求共享状态」的竞态；引擎侧只读不写全局。
type RunControl struct {
	mu         sync.Mutex
	budget     int             // 本次调用最多执行几个节点；<0 表示不限
	stopBefore map[string]bool // 断点节点集合
	hit        map[string]bool // 已命中过的断点（同一次调用内不重复停）
	paused     bool
	pausedAt   string
	reason     string // step / breakpoint
	executed   int

	// agentConcurrency 本次运行的智能体并发上限；0 = 用引擎默认（AGENT_MAX_CONCURRENCY / 2）。
	//
	// 为什么配额是**运行级**而不是引擎级：同一张图在不同机器、不同 agent 上能扛的并发数
	// 完全不同（实测 dsh headless 一个约 300MB，Claude Code 是几百 MB 到几 GB）。
	// 引擎级硬编码会让「三路并行」的图第三路白等到前一路跑完——
	// 2026-10-23 基准里第三路白等 58s，整图 135.6s，而理论值只要 ~85s。
	agentConcurrency int
}

// NewStepControl 单步控制：本次最多执行 budget 个节点后停下
func NewStepControl(budget int) *RunControl {
	if budget < 0 {
		budget = -1
	}
	return &RunControl{budget: budget, hit: map[string]bool{}}
}

// NewFullControl 全速执行（不单步、不断点）。
// 用于「只想带运行级参数（如 agent 并发），不想接管节点推进」的场景。
func NewFullControl() *RunControl {
	return &RunControl{budget: -1, hit: map[string]bool{}}
}

// NewBreakpointControl 断点控制：执行到断点节点前停下；stop 为空且 budget<0 即全速
func NewBreakpointControl(stop map[string]bool) *RunControl {
	cp := make(map[string]bool, len(stop))
	for k, v := range stop {
		if v && k != "" {
			cp[k] = true
		}
	}
	return &RunControl{budget: -1, stopBefore: cp, hit: map[string]bool{}}
}

// WithBudget 设置本次调用的节点预算（用于 step 连续推进时复用同一个控制对象）
func (rc *RunControl) WithBudget(n int) *RunControl {
	rc.mu.Lock()
	rc.budget = n
	rc.mu.Unlock()
	return rc
}

// ShouldPauseBefore 命中未触发过的断点则返回 true（并标记已命中）
func (rc *RunControl) ShouldPauseBefore(nodeID string) bool {
	if rc == nil {
		return false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.stopBefore) == 0 || !rc.stopBefore[nodeID] || rc.hit[nodeID] {
		return false
	}
	rc.hit[nodeID] = true
	return true
}

// Take 申请一个执行许可；预算耗尽返回 false
func (rc *RunControl) Take() bool {
	if rc == nil {
		return true
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.budget < 0 {
		rc.executed++
		return true
	}
	if rc.executed >= rc.budget {
		return false
	}
	rc.executed++
	return true
}

// MarkPaused 记录暂停点（节点 ID 可为空，表示「预算用尽」）
func (rc *RunControl) MarkPaused(nodeID, reason string) {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	if !rc.paused {
		rc.paused = true
		rc.pausedAt = nodeID
		rc.reason = reason
	}
	rc.mu.Unlock()
}

// State 返回暂停状态：是否暂停 / 停在哪个节点 / 原因
func (rc *RunControl) State() (paused bool, at string, reason string) {
	if rc == nil {
		return false, "", ""
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.paused, rc.pausedAt, rc.reason
}

// Reset 复位暂停标记，便于同一控制对象连续推进
func (rc *RunControl) Reset() {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	rc.paused = false
	rc.pausedAt = ""
	rc.reason = ""
	rc.mu.Unlock()
}

// AgentConcurrency 本次运行声明的 agent 并发上限；0 表示沿用引擎默认
func (rc *RunControl) AgentConcurrency() int {
	if rc == nil {
		return 0
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.agentConcurrency
}

// SetAgentConcurrency 设置本次运行的 agent 并发上限（1..16，越界按 0 处理 = 用默认）
func (rc *RunControl) SetAgentConcurrency(n int) *RunControl {
	if rc == nil {
		return nil
	}
	if n < 1 || n > 16 {
		n = 0
	}
	rc.mu.Lock()
	rc.agentConcurrency = n
	rc.mu.Unlock()
	return rc
}

type ctxKeyRunControl struct{}

func withRunControl(ctx context.Context, rc *RunControl) context.Context {
	if rc == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRunControl{}, rc)
}

func runControlFrom(ctx context.Context) *RunControl {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(ctxKeyRunControl{}).(*RunControl); ok {
		return v
	}
	return nil
}
