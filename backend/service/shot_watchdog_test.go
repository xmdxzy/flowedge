package service

import (
	"sync/atomic"
	"testing"
	"time"
)

// 阈值必须落在「健康区间之上、换页区间之下」。
// 按 2026-10-26 重新标定的基准（4.5 秒/帧）：健康 0.9–2.0 倍、换页 3.4–10.7 倍；
// 5 倍正好落在两者中间，两头都留了余量。
func TestShotGenerationLimit(t *testing.T) {
	frames := 56
	// 56 帧 × 4.5 ≈ 252s
	base := time.Duration(shotCostSeconds(frames, shotBaseWidth, shotBaseHeight, shotBaseSteps) * float64(time.Second))
	got := ShotGenerationLimit(frames, shotBaseWidth, shotBaseHeight, shotBaseSteps)
	want := time.Duration(float64(base) * shotAbortFactor)
	if got != want {
		t.Fatalf("上限 = %s，want %s", got, want)
	}
	// 实测：健康 1.5 倍不该被掐；11 倍必须被掐
	if got <= time.Duration(float64(base)*1.5) {
		t.Fatalf("上限 %s 太紧，会把健康的长任务误杀", got)
	}
	if got > time.Duration(float64(base)*10) {
		t.Fatalf("上限 %s 太松，掐不住换页那种量级", got)
	}
	if ShotGenerationLimit(0, 608, 352, 12) != 0 {
		t.Fatal("帧数未知时不该设限")
	}
}

// 高分辨率档的上限必须跟着放大，否则会误杀真实任务。
// 依据：run 217 / 222 用的是 768×448 @20 步，单镜成本约 608×352@12 的 2.5 倍。
func TestShotGenerationLimit_ScalesWithResolution(t *testing.T) {
	low := ShotGenerationLimit(56, 608, 352, 12)
	high := ShotGenerationLimit(56, 768, 448, 20)
	if high <= low*2 {
		t.Fatalf("768×448@20 的上限 %s 只比 608×352@12 的 %s 大一点，会误杀", high, low)
	}
	if high >= low*3 {
		t.Fatalf("放大过头：%s vs %s（像素比 1.6 × 步数比 1.67 ≈ 2.7 倍）", high, low)
	}
}

// 到点必须 fire 一次，且 Aborted() 变 true。
func TestWatchdog_FiresOnce(t *testing.T) {
	var fired atomic.Int32
	w := armShotWatchdog(60*time.Millisecond, func() { fired.Add(1) })
	time.Sleep(180 * time.Millisecond)
	w.Stop()
	if n := fired.Load(); n != 1 {
		t.Fatalf("到点应只触发 1 次，实际 %d", n)
	}
	if !w.Aborted() {
		t.Fatal("触发后 Aborted() 应为 true")
	}
}

// 生成提前返回（正常情况）不能误触发。
func TestWatchdog_StoppedBeforeTimeout(t *testing.T) {
	var fired atomic.Int32
	w := armShotWatchdog(2*time.Second, func() { fired.Add(1) })
	time.Sleep(20 * time.Millisecond)
	w.Stop()
	// Stop 之后即使再等，也不许 fire
	time.Sleep(120 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Fatalf("正常返回不该触发看门狗，实际 %d 次", n)
	}
	if w.Aborted() {
		t.Fatal("没触发过 Aborted() 必须是 false")
	}
}

// 排队时间不该计入生成时限：run 224 就是排队 6 分钟把健康的镜头耗到 5 倍基准上的。
func TestWatchdog_PauseExcludesQueueWait(t *testing.T) {
	var fired atomic.Int32
	w := armShotWatchdog(400*time.Millisecond, func() { fired.Add(1) })
	w.Pause()
	time.Sleep(900 * time.Millisecond) // 排队 900ms，一个 tick 都不该算进 active
	if fired.Load() != 0 {
		t.Fatalf("排队期间不该触发，实际 %d 次", fired.Load())
	}
	w.Resume()
	time.Sleep(200 * time.Millisecond) // 累计不足 400ms
	if fired.Load() != 0 {
		t.Fatalf("恢复后不足上限不该触发，实际 %d 次", fired.Load())
	}
	time.Sleep(450 * time.Millisecond)
	if fired.Load() == 0 {
		t.Fatal("恢复后累计满上限必须触发")
	}
	if !w.Aborted() {
		t.Fatal("触发后 Aborted() 应为 true")
	}
	w.Stop()
}

// Stop 必须可重复调用，且不阻塞（多次 Stop 的调用方可能来自 defer）。
func TestWatchdog_StopIsIdempotent(t *testing.T) {
	w := armShotWatchdog(time.Second, nil)
	w.Stop()
	w.Stop()
	w.Stop()
}

// limit<=0（帧数未知）时是空操作，Stop 不能挂住。
func TestWatchdog_Disabled(t *testing.T) {
	var fired atomic.Int32
	w := armShotWatchdog(0, func() { fired.Add(1) })
	w.Stop()
	if fired.Load() != 0 || w.Aborted() {
		t.Fatal("不设限时不该触发")
	}
}
