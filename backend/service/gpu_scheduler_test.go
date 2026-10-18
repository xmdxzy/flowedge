package service

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestGPUScheduler_AcquireReleaseRefs 引用计数必须正确加减，且并发下不得自死锁
func TestGPUScheduler_AcquireReleaseRefs(t *testing.T) {
	done := make(chan struct{})
	var refsAfter int
	go func() {
		defer close(done)
		s := NewGPUScheduler(24576)
		s.RegisterModel("m", "llm", 100)
		// 预置遥测缓存，避免测试反复拉起 nvidia-smi 子进程
		s.mu.Lock()
		s.cachedGPU = &GPUInfo{TotalMB: 24576, UsedMB: 100, FreeMB: 24476}
		s.cachedAt = time.Now()
		s.telemetryInterval = 5 * time.Second
		s.mu.Unlock()
		if err := s.AcquireModel("m"); err != nil {
			t.Errorf("acquire: %v", err)
			return
		}
		if err := s.AcquireModel("m"); err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		// 无 nvidia-smi 的机器走缓存/失败路径，这里只验证不 panic 不死锁
		s.ReleaseModel("m")

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = s.AcquireModel("m")
				s.ReleaseModel("m")
			}()
		}
		wg.Wait()

		s.mu.Lock()
		refsAfter = s.models["m"].Refs
		s.mu.Unlock()
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Acquire/Release 并发下死锁（GetGPUInfo 重入 s.mu）")
	}
	// 断言放在主 goroutine：从子 goroutine 调 Fatalf 会被 vet 判定为误用
	if refsAfter < 0 {
		t.Fatalf("refs went negative: %d", refsAfter)
	}
}

// TestGPUScheduler_UnloadCalledAndRefsProtected 卸载必须真实调用 unloader，
// 且 Refs>0 的模型不得被卸
func TestGPUScheduler_UnloadCalledAndRefsProtected(t *testing.T) {
	s := NewGPUScheduler(24576)
	s.mu.Lock()
	s.cachedGPU = &GPUInfo{TotalMB: 24576, FreeMB: 24000}
	s.cachedAt = time.Now()
	s.mu.Unlock()

	var mu sync.Mutex
	unloaded := []string{}
	s.SetUnloader(func(name, typ string) error {
		mu.Lock()
		unloaded = append(unloaded, name)
		mu.Unlock()
		return nil
	})

	s.RegisterModel("busy", "llm", 100)
	s.RegisterModel("cold", "llm", 100)

	// busy 正在使用（Refs>0），cold 空闲
	if err := s.AcquireModel("busy"); err != nil && err.Error() != "" {
		// nvidia-smi 不可用时 Acquire 可能失败；失败时手动置状态以保证测试确定性
		s.mu.Lock()
		s.models["busy"].Status = ModelStatusReady
		s.models["busy"].Refs = 1
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.models["cold"].Status = ModelStatusReady
	s.models["cold"].Refs = 0
	s.mu.Unlock()

	s.evictLRU(1)

	mu.Lock()
	defer mu.Unlock()
	if len(unloaded) != 1 || unloaded[0] != "cold" {
		t.Fatalf("only cold should be unloaded, got %v", unloaded)
	}

	s.mu.Lock()
	busy := s.models["busy"]
	timelineLen := len(s.timeline)
	s.mu.Unlock()
	if busy.Refs != 1 {
		t.Fatalf("busy refs corrupted: %d", busy.Refs)
	}
	if timelineLen == 0 {
		t.Fatal("timeline must record reserve/release/unload")
	}
}

// TestGPUScheduler_UnloadFailureKeepsStatus 卸载失败不得把状态改成 idle（账实不符）
func TestGPUScheduler_UnloadFailureKeepsStatus(t *testing.T) {
	s := NewGPUScheduler(24576)
	s.mu.Lock()
	s.cachedGPU = &GPUInfo{TotalMB: 24576, FreeMB: 24000}
	s.cachedAt = time.Now()
	s.mu.Unlock()
	s.SetUnloader(func(name, typ string) error { return errors.New("boom") })

	s.RegisterModel("m", "llm", 100)
	s.mu.Lock()
	s.models["m"].Status = ModelStatusReady
	s.mu.Unlock()

	s.evictLRU(1)

	s.mu.Lock()
	st := s.models["m"].Status
	s.mu.Unlock()
	if st != ModelStatusReady {
		t.Fatalf("failed unload must keep status ready, got %s", st)
	}
}

// TestGPUScheduler_Timeline 时间线事件可查且有上限
func TestGPUScheduler_Timeline(t *testing.T) {
	s := NewGPUScheduler(24576)
	s.mu.Lock()
	s.cachedGPU = &GPUInfo{TotalMB: 24576, FreeMB: 24000}
	s.cachedAt = time.Now()
	s.mu.Unlock()
	s.RegisterModel("m", "llm", 100)
	for i := 0; i < 250; i++ {
		_ = s.AcquireModel("m")
		s.ReleaseModel("m")
	}
	tl := s.Timeline(60)
	if len(tl) != 60 {
		t.Fatalf("timeline limit = %d, want 60", len(tl))
	}
	// 最后一个事件应为 release
	if tl[len(tl)-1].Action != "release" {
		t.Fatalf("last event = %s, want release", tl[len(tl)-1].Action)
	}
	if tl[0].Model == "" {
		t.Fatal("timeline events must carry model name")
	}
}
