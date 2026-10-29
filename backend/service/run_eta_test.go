package service

import (
	"testing"
	"time"
)

// 本次运行的 ETA 预测：第一次跑也能给出剩余时间（不依赖历史均值）。
//
// 由来（2026-10-26）：实测 run_events 里 node_progress 的 eta_ms 长期恒为 0，
// 因为 ETAFor 只用"同类节点历史 EWMA"，第一次跑必然没有样本 —— 进度条只能空转。
// shotlist 的工作量是可以直接从分镜表算出来的，所以给它一条预测通道。
func TestRunPrediction(t *testing.T) {
	e := &EngineV2{}

	// 没登记 → 没有预测（调用方退回历史均值）
	if _, ok := e.predictedRemaining(123, 5*time.Second); ok {
		t.Fatal("未登记时不该有预测")
	}

	e.SetRunPrediction(123, time.Minute)
	if got, ok := e.predictedRemaining(123, 20*time.Second); !ok || got != 40000 {
		t.Fatalf("剩余应为 40s，实际 %dms ok=%v", got, ok)
	}
	// 超时之后剩余夹到 0，不能给负数（前端会显示"-3 分钟"）
	if got, ok := e.predictedRemaining(123, 5*time.Minute); !ok || got != 0 {
		t.Fatalf("超出预测后应夹到 0，实际 %d ok=%v", got, ok)
	}
	// 只影响登记过的那次运行
	if _, ok := e.predictedRemaining(999, time.Second); ok {
		t.Fatal("别的 run 不该共用预测")
	}

	e.ClearRunPrediction(123)
	if _, ok := e.predictedRemaining(123, time.Second); ok {
		t.Fatal("清理后不该再有预测")
	}
	// 边界：runID=0 / 非正时长都不登记（独立执行没有 run）
	e.SetRunPrediction(0, time.Minute)
	e.SetRunPrediction(7, 0)
	if _, ok := e.predictedRemaining(7, time.Second); ok {
		t.Fatal("非正时长不该登记")
	}
}
