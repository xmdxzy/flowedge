package service

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 基准档（608×352 / 12 步）的单镜健康成本。
func baseCost(frames int) time.Duration {
	return time.Duration(shotCostSeconds(frames, shotBaseWidth, shotBaseHeight, shotBaseSteps) * float64(time.Second))
}

// 慢镜判据的边界：3 倍才喊，2.9 倍不喊。
func TestJudgeShotSpeed_Threshold(t *testing.T) {
	frames := 73 // 基准 73×4.5 = 328.5s
	base := baseCost(frames)
	if msg := JudgeShotSpeed(frames, 608, 352, 12, base, 20000); msg != "" {
		t.Fatalf("正好等于基准不该告警，实际：%s", msg)
	}
	if msg := JudgeShotSpeed(frames, 608, 352, 12, base*5/2, 20000); msg != "" {
		t.Fatalf("2.5 倍（还在抖动范围）不该告警，实际：%s", msg)
	}
	msg := JudgeShotSpeed(frames, 608, 352, 12, base*3, 20000)
	if msg == "" {
		t.Fatal("3 倍必须告警")
	}
	if !strings.Contains(msg, "3.0 倍") {
		t.Fatalf("告警要带倍数，实际：%s", msg)
	}
}

// 那一晚的真实数字：84.9s → 2269.8s 必须被判成异常，并点出换页。
// 按 4.5 秒/帧 的新基准，2269s / 328.5s ≈ 6.9 倍（旧基准下是 26 倍）——两种算法都远超 3 倍红线。
func TestJudgeShotSpeed_RealIncident(t *testing.T) {
	frames := 73
	msg := JudgeShotSpeed(frames, 608, 352, 12, 2269*time.Second, 752)
	if msg == "" {
		t.Fatal("2269s / 73 帧 ≈ 6.9 倍，必须告警")
	}
	// 倍数按实现同一套算术推，避免把常量变化误当成回归
	base := baseCost(frames)
	wantRatio := fmt.Sprintf("%.1f 倍", (2269*time.Second).Seconds()/base.Seconds())
	for _, want := range []string{wantRatio, "752MB", "换页"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("告警里应有 %q，实际：%s", want, msg)
		}
	}
}

// 拿不到显存读数时也要告警，但不能瞎断言成因。
func TestJudgeShotSpeed_NoFreeReading(t *testing.T) {
	// 1500s / 328.5s ≈ 4.6 倍（基准 73 帧 × 4.5）——明显异常，必须喊，
	// 且因为没有显存读数，只能说"拿不到"而不能断言是换页。
	msg := JudgeShotSpeed(73, 608, 352, 12, 1500*time.Second, 0)
	if msg == "" {
		t.Fatal("7 倍慢必须告警")
	}
	if !strings.Contains(msg, "拿不到") {
		t.Fatalf("没有读数时要说清是拿不到，而不是断言换页：%s", msg)
	}
}

// 帧数未知时不做判断（宁可不喊，也不喊错）。
func TestJudgeShotSpeed_UnknownFrames(t *testing.T) {
	if msg := JudgeShotSpeed(0, 608, 352, 12, 10*time.Minute, 100); msg != "" {
		t.Fatalf("帧数未知不该判断，实际：%s", msg)
	}
}

// 高分辨率档不能沿用基准档的判据：768×448@20 步 每帧成本是基准的 ~2.6 倍，
// 用基准算会把健康的镜头全判成"慢 3 倍"。
func TestJudgeShotSpeed_RespectsResolution(t *testing.T) {
	frames := 56
	// 这个耗时是基准档的 3.5 倍：基准档下必须告警；高分辨率档（成本 ≈2.6 倍）下只有 1.35 倍
	elapsed := baseCost(frames) * 7 / 2
	if msg := JudgeShotSpeed(frames, 608, 352, 12, elapsed, 20000); msg == "" {
		t.Fatal("同一耗时在基准档下应该被判慢")
	}
	if msg := JudgeShotSpeed(frames, 768, 448, 20, elapsed, 20000); msg != "" {
		t.Fatalf("高分辨率档下同样的耗时属于正常，不该告警：%s", msg)
	}
}

// 预算公式与告警判据必须共用同一个成本模型，否则会各自漂移。
func TestShotListBudget_UsesSharedConstant(t *testing.T) {
	// 2 镜 × 73 帧：146×4.5 + 2×90 + 600 = 1437s（23m57s）
	frames := 146
	got := ShotListBudget(2, frames, 608, 352, 12)
	want := baseCost(frames) + 180*time.Second + 10*time.Minute
	if got != want {
		t.Fatalf("预算 = %s，按共享常数应为 %s", got, want)
	}
	// 这里原来钉死 955s（146×1.2+180+600），但那正是「预算少给」那次事故的产物：
	// run 205 的 15m55s 不是合理预算，而是按过期基准（1.2 秒/帧）算出来的偏小值，
	// 长镜因此必然撞节点超时。现在改为断言「预算盖得住各镜健康成本之和」——
	// 这才是那条 bug 的教训，而且以后调 shotSecPerFrame 不必再改这个测试。
	if got < baseCost(frames) {
		t.Fatalf("预算 %s 小于两镜健康成本之和 %s，长镜必然撞节点超时", got, baseCost(frames))
	}
}

// 同一帧数、不同分辨率/步数，预算必须跟着放大（否则长镜必然撞节点超时）。
// 注意固定开销（装配 90s/镜 + 10min）不随参数变，所以比较的是**可变部分**。
func TestShotListBudget_ScalesWithParams(t *testing.T) {
	const fixed = 180*time.Second + 10*time.Minute
	low := ShotListBudget(2, 146, 608, 352, 12)
	high := ShotListBudget(2, 146, 768, 448, 20)
	if high <= low {
		t.Fatalf("768×448@20 的预算 %s 不应小于 608×352@12 的 %s", high, low)
	}
	lowVar, highVar := low-fixed, high-fixed
	ratio := float64(highVar) / float64(lowVar)
	if ratio < 2.2 || ratio > 3.2 {
		t.Fatalf("可变预算放大 %.2f 倍，像素比 1.6 × 步数比 1.67 ≈ 2.67 才对（low=%s high=%s）",
			ratio, lowVar, highVar)
	}
}
