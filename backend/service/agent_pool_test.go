package service

import (
	"context"
	"testing"
	"time"
)

// 配额池必须按运行隔离，而且必须真的限制住第 N+1 个。
//
// 背景（2026-10-23 基准）：配额原来是引擎级硬编码 2，三路并行的图第三路白等
// 前一路跑完 —— 整图 135.6s，理论值 ~85s。改成运行级参数后必须保证两件事：
//   1. 声明了 agent_concurrency=3 就真能同时进 3 个；
//   2. 引擎默认池不受影响（没声明时行为与改动前一致）。
func TestAgentPoolHonorsRunQuota(t *testing.T) {
	e := &EngineV2{}
	ctx := withAgentPool(context.Background(), 3)

	held := make([]*agentPool, 0, 3)
	for i := 0; i < 3; i++ {
		p, err := e.acquireAgentSlot(ctx)
		if err != nil {
			t.Fatalf("第 %d 个名额申请失败：%v", i+1, err)
		}
		held = append(held, p)
	}

	// 第 4 个必须等：用一个立刻超时的 ctx，应该拿到超时错误而不是名额
	blocked, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err := e.acquireAgentSlot(blocked); err == nil {
		t.Fatal("配额 3 却发出了第 4 个名额：池没有真的限制并发")
	}

	// 归还一个后应能再拿到
	held[0].release()
	again, cancel2 := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel2()
	p4, err := e.acquireAgentSlot(again)
	if err != nil {
		t.Fatalf("归还名额后仍拿不到：%v", err)
	}
	p4.release()

	for _, p := range held[1:] {
		p.release()
	}
}

// 没声明配额时退回引擎默认池，且默认池不会因为一次运行被「借走」而变小。
func TestAgentPoolFallsBackToEngineDefault(t *testing.T) {
	t.Setenv("AGENT_MAX_CONCURRENCY", "2")
	e := &EngineV2{}
	ctx := context.Background()

	p1, err := e.acquireAgentSlot(ctx)
	if err != nil {
		t.Fatalf("默认池申请失败：%v", err)
	}
	if p1.size != 2 {
		t.Fatalf("默认池容量应为 2，实际 %d", p1.size)
	}
	p1.release()

	// 运行级池存在时，默认池不应被占用
	runCtx := withAgentPool(context.Background(), 1)
	rp, err := e.acquireAgentSlot(runCtx)
	if err != nil {
		t.Fatalf("运行级池申请失败：%v", err)
	}
	if rp.size != 1 {
		t.Fatalf("运行级池容量应为 1，实际 %d", rp.size)
	}
	if rp == p1 {
		t.Fatal("运行级池与引擎默认池是同一个对象：配额没有按运行隔离")
	}
	rp.release()
}

// RunControl 的并发声明必须可读可写，且越界值退回默认而不是把配额设成 0 个。
func TestRunControlAgentConcurrency(t *testing.T) {
	rc := NewFullControl()
	if got := rc.AgentConcurrency(); got != 0 {
		t.Fatalf("新建控制对象默认应为 0（用引擎默认），实际 %d", got)
	}
	rc.SetAgentConcurrency(3)
	if got := rc.AgentConcurrency(); got != 3 {
		t.Fatalf("应记住 3，实际 %d", got)
	}
	rc.SetAgentConcurrency(99)
	if got := rc.AgentConcurrency(); got != 0 {
		t.Fatalf("越界值应退回 0（表示用默认），实际 %d", got)
	}

	var nilRC *RunControl
	if got := nilRC.AgentConcurrency(); got != 0 {
		t.Fatalf("nil 控制对象应返回 0，实际 %d", got)
	}
}
