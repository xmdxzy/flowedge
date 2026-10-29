package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mockTTS 起一个假的 TTS 服务：/health 报指定显存读数，/unload 记调用次数。
//
// 为什么用 HTTP 而不是接口 mock：回收器的判据来自真实服务的 /health vram 段
// （reserved 才是判据，不是 allocated），假服务能钉住这个契约形状。
func mockTTS(t *testing.T, residentMB int) (*httptest.Server, *int32) {
	t.Helper()
	var unloads int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"loaded": true,
			"vram": map[string]int{
				"allocated_mb": residentMB,
				"reserved_mb":  residentMB,
				"free_mb":      7283,
				"total_mb":     24575,
			},
		})
	})
	mux.HandleFunc("/unload", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&unloads, 1)
		_, _ = w.Write([]byte(`{"ok":true,"unloaded":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &unloads
}

func newIdleTestEngine(ttsURL string) *EngineV2 {
	return &EngineV2{
		tts:        NewTTSService(ttsURL),
		activeRuns: map[uint]context.CancelFunc{},
	}
}

func withCPUFlag(t *testing.T, cpu bool) {
	t.Helper()
	orig := ttsOnCPU
	ttsOnCPU = func() bool { return cpu }
	t.Cleanup(func() { ttsOnCPU = orig })
}

// TestIdleReclaimerRespectsThreshold 阈值前不动手，到点只动手一次。
func TestIdleReclaimerRespectsThreshold(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: 10 * time.Minute, TTSMinFreeMB: 10000, TTSGuard: true,
	})

	t0 := time.Now()
	r.Tick(t0) // 记下空闲起点
	if got := r.LastResult(); got != nil {
		t.Fatalf("第一次 tick 不该回收，却拿到 %+v", got)
	}

	r.Tick(t0.Add(9 * time.Minute))
	if got := r.LastResult(); got != nil {
		t.Fatalf("未到 10 分钟不该回收，却拿到 %+v", got)
	}
	if n := atomic.LoadInt32(unloads); n != 0 {
		t.Fatalf("未到阈值就调了 /unload %d 次", n)
	}

	r.Tick(t0.Add(11 * time.Minute))
	res := r.LastResult()
	if res == nil {
		t.Fatal("到点应该回收，但没有任何结果")
	}
	if len(res.Done) == 0 {
		t.Fatalf("期望这次回收了 tts，实际 Done 为空：%+v", res)
	}
	if n := atomic.LoadInt32(unloads); n != 1 {
		t.Fatalf("期望 /unload 被调 1 次，实际 %d 次", n)
	}
}

// TestIdleReclaimerNoRepeat 一轮空闲里不许每 30s 重复发卸载请求。
func TestIdleReclaimerNoRepeat(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: time.Minute, TTSMinFreeMB: 10000, TTSGuard: true,
	})

	t0 := time.Now()
	r.Tick(t0)
	for i := 1; i <= 5; i++ {
		r.Tick(t0.Add(time.Duration(i) * time.Minute))
	}
	if n := atomic.LoadInt32(unloads); n != 1 {
		t.Fatalf("同一轮空闲只该卸 1 次，实际 %d 次", n)
	}
}

// TestIdleReclaimerBusyResets 有活进来必须重置计时，不能把正在排的片子打断。
func TestIdleReclaimerBusyResets(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: 10 * time.Minute, TTSMinFreeMB: 10000, TTSGuard: true,
	})

	t0 := time.Now()
	r.Tick(t0)
	r.Tick(t0.Add(9 * time.Minute))

	// 模拟一个 run 起飞
	e.activeRuns[1] = func() {}
	r.Tick(t0.Add(9*time.Minute + 30*time.Second))

	// 之后又空闲：计时重新开始，不能因为「累计空闲超过 10 分钟」就立刻卸
	delete(e.activeRuns, 1)
	r.Tick(t0.Add(10 * time.Minute))
	if got := r.LastResult(); got != nil {
		t.Fatalf("忙碌后刚回到空闲就回收了，计时没重置：%+v", got)
	}
	if n := atomic.LoadInt32(unloads); n != 0 {
		t.Fatalf("不该有任何 /unload，实际 %d 次", n)
	}

	// 从新的空闲起点算满 10 分钟才该卸
	r.Tick(t0.Add(20*time.Minute + 31*time.Second))
	if n := atomic.LoadInt32(unloads); n != 1 {
		t.Fatalf("计时重置后满 10 分钟应卸 1 次，实际 %d 次", n)
	}
}

// TestIdleReclaimTTSGuardSkipsWhenCannotReload 守卫：卸了就装不回来时，宁可不卸。
//
// 这是整条策略的安全阀。没有它，用户会在「开着 UE 编辑器」的那天遇到：
// 空闲时模型被卸 → 下次配音 preflight 拒绝 → 必须先手动关程序才能用。
func TestIdleReclaimTTSGuardSkipsWhenCannotReload(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	// 无 gpuSched ⇒ deviceFreeMB 读作 0；0 + 14996 < 20787 ⇒ 判定「装不回来」
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: time.Minute, TTSMinFreeMB: 20787, TTSGuard: true,
	})

	t0 := time.Now()
	r.Tick(t0)
	r.Tick(t0.Add(2 * time.Minute))

	res := r.LastResult()
	if res == nil {
		t.Fatal("应当产出一个「跳过」结果")
	}
	if len(res.Done) != 0 {
		t.Fatalf("守卫生效时不该卸任何东西，实际 Done=%v", res.Done)
	}
	if n := atomic.LoadInt32(unloads); n != 0 {
		t.Fatalf("守卫生效时不该调 /unload，实际 %d 次", n)
	}
	found := false
	for _, s := range res.Skipped {
		if len(s) > 0 && (contains(s, "装不回来")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("跳过原因里应说明「装不回来」，实际 %v", res.Skipped)
	}
}

// TestIdleReclaimTTSGuardOffUnloadsAnyway 守卫关掉 = 接受「为了腾地方牺牲下次可用性」。
func TestIdleReclaimTTSGuardOffUnloadsAnyway(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: time.Minute, TTSMinFreeMB: 20787, TTSGuard: false,
	})

	t0 := time.Now()
	r.Tick(t0)
	r.Tick(t0.Add(2 * time.Minute))

	if n := atomic.LoadInt32(unloads); n != 1 {
		t.Fatalf("守卫关闭时应卸 1 次，实际 %d 次", n)
	}
}

// TestIdleReclaimSkipsCPUTTS CPU 上的 TTS 一个字节显存都不占，不该被惊动。
func TestIdleReclaimSkipsCPUTTS(t *testing.T) {
	withCPUFlag(t, true)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{
		After: time.Minute, TTSMinFreeMB: 10000, TTSGuard: true,
	})

	t0 := time.Now()
	r.Tick(t0)
	r.Tick(t0.Add(2 * time.Minute))

	if n := atomic.LoadInt32(unloads); n != 0 {
		t.Fatalf("CPU 上的 TTS 不该被卸载，实际调用 %d 次", n)
	}
}

// TestIdleReclaimerDisabled 配置成关闭时，永远不动手。
func TestIdleReclaimerDisabled(t *testing.T) {
	withCPUFlag(t, false)
	srv, unloads := mockTTS(t, 14996)
	e := newIdleTestEngine(srv.URL)
	r := NewIdleReclaimer(e, nil, IdleReclaimPolicy{After: 0, TTSGuard: true})

	t0 := time.Now()
	r.Tick(t0)
	r.Tick(t0.Add(24 * time.Hour))
	if n := atomic.LoadInt32(unloads); n != 0 {
		t.Fatalf("关闭状态下不该调用 /unload，实际 %d 次", n)
	}
	if r.LastResult() != nil {
		t.Fatalf("关闭状态下不该有结果：%+v", r.LastResult())
	}
}

// contains 小工具（避免为一行引入 strings 依赖歧义）。
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
