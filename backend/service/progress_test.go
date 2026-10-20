package service

import (
	"context"
	"testing"
	"time"

	"flowedge/backend/model"
)

// TestNodeProgressHeartbeat 验证 P0 可观测性三件事：
//  1. 长任务执行期间会持续推 node_progress 心跳，且带 elapsed_ms
//  2. node_finished 带 duration_ms（前端时间线要用）
//  3. 跑过一次之后，同类型节点的 ETAFor 能给出非零预计耗时
func TestNodeProgressHeartbeat(t *testing.T) {
	// 心跳间隔默认 2s，测试里调快，避免真等两秒
	old := nodeProgressTick
	nodeProgressTick = 20 * time.Millisecond
	defer func() { nodeProgressTick = old }()

	hub := NewWSHub()
	events := hub.Subscribe()

	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, hub)
	e.RegisterNodeType("slow", func(_ context.Context, _ *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		time.Sleep(150 * time.Millisecond)
		return map[string]interface{}{"text": "done"}, nil
	})

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "seed"}},
		{"id": "w1", "type": "slow", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "w1"},
		{"id": "b", "source": "w1", "target": "e1"},
	})

	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	var progressCount int
	var finishDur int64
	for done := false; !done; {
		select {
		case ev := <-events:
			switch ev.Type {
			case "node_progress":
				if ev.NodeID != "w1" {
					continue
				}
				progressCount++
				ms, _ := ev.Data["elapsed_ms"].(int64)
				if ms <= 0 {
					t.Errorf("node_progress elapsed_ms = %v, want > 0", ev.Data["elapsed_ms"])
				}
			case "node_finished":
				if ev.NodeID == "w1" {
					finishDur, _ = ev.Data["duration_ms"].(int64)
				}
			}
		default:
			done = true
		}
	}

	if progressCount == 0 {
		t.Fatal("没有收到任何 node_progress 心跳事件——长任务仍会是盲等")
	}
	if finishDur < 100 {
		t.Errorf("node_finished duration_ms = %d, want >= 100（节点实际睡了 150ms）", finishDur)
	}
	if got := e.ETAFor("slow"); got <= 0 {
		t.Errorf("ETAFor(slow) = %d, want > 0（已跑过一次应有历史样本）", got)
	}
}

// TestETAForUnknownTypeIsZero 没有历史样本时不该编造 ETA，前端据此隐藏预计时间。
func TestETAForUnknownTypeIsZero(t *testing.T) {
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if got := e.ETAFor("never_ran"); got != 0 {
		t.Errorf("ETAFor(never_ran) = %d, want 0", got)
	}
}
