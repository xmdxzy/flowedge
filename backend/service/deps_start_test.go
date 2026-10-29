package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// newBareSupervisor 造一个只含注入规格的 Supervisor（不读 data/deps.json）。
func newBareSupervisor() *Supervisor {
	return &Supervisor{
		specs:      map[string]*DepSpec{},
		procs:      map[string]*exec.Cmd{},
		started:    map[string]bool{},
		lastErr:    map[string]string{},
		procLogs:   map[string][]*os.File{},
		startLocks: map[string]*sync.Mutex{},
		client:     &http.Client{Timeout: 800 * time.Millisecond},
	}
}

// 已就绪的依赖必须直接返回，绝不能再派生一个实例。
//
// 这是 2026-10-26 那晚的直接教训：8189 上有两个 ComfyUI-H3 进程，
// 一个抢到端口持有 20GB，另一个卡在 3MB 活了一小时；三次出片全部 15 分钟超时，
// 单镜从 84.9s 退化到 2269.8s——就是「两个实例抢一张 24G 卡」。
// 这里的假依赖把 Binary 指到一个必然不存在的路径：只要它真去 exec 就会报错，
// 所以「返回 nil」本身就证明没有第二次派生。
func TestStart_AlreadyHealthyDoesNotSpawnAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[{"vram_free":1}]}`))
	}))
	defer srv.Close()

	s := newBareSupervisor()
	s.specs["fake"] = &DepSpec{
		Name: "fake", Label: "假依赖",
		Binary:     "/nonexistent/definitely-not-a-binary",
		Endpoint:   srv.URL,
		HealthPath: "/system_stats",
	}
	if err := s.Start("fake"); err != nil {
		t.Fatalf("已就绪的依赖应当直接返回 nil，实际：%v", err)
	}
}

// 单飞锁：同一个依赖的两次并发启动必须串行，不会同时进入派生段。
func TestStartLock_SerializesSameDep(t *testing.T) {
	s := newBareSupervisor()

	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := s.startLock("comfy_h3")
			lock.Lock()
			defer lock.Unlock()
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("同一依赖的启动段最多只能有 1 个 goroutine，实际峰值 %d", maxInside)
	}
}

// 不同依赖之间不许互相阻塞：装 H3 的时候不该把 ASR 的启动也卡住。
func TestStartLock_DifferentDepsDoNotBlock(t *testing.T) {
	s := newBareSupervisor()
	start := make(chan struct{})
	done := make(chan struct{})

	lockA := s.startLock("comfy_h3")
	lockA.Lock()

	go func() {
		lockB := s.startLock("asr")
		lockB.Lock()
		close(done)
		lockB.Unlock()
	}()

	select {
	case <-done:
	case <-start:
	case <-time.After(2 * time.Second):
		t.Fatal("asr 的启动被 comfy_h3 的锁挡住了（锁粒度错了）")
	}
	lockA.Unlock()
}

// 「忙」绝不能报成「未启动」。
//
// 2026-10-26 实测：H3 换页时 GPU 100%、占 23.8GB，但 /system_stats 与 /queue
// 全部超时不回 → 旧逻辑 Running=false → 面板显示「未启动」→ 用户点「启动」
// → 又派生一份实例，两个进程抢一张卡。这就是那晚 27 倍慢的起点。
func TestStatus_BusyIsNotStopped(t *testing.T) {
	dead := "http://127.0.0.1:1" // 连不上：模拟「忙到不回健康检查」

	s := newBareSupervisor()
	s.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint: dead, HealthPath: "/system_stats",
	}
	// 我们派生的进程还活着（Pid 有效）
	s.mu.Lock()
	s.procs["comfy_h3"] = &exec.Cmd{Process: &os.Process{Pid: 4242}}
	s.mu.Unlock()

	st := s.Status()
	if len(st) != 1 {
		t.Fatalf("状态条目数应为 1，实际 %d", len(st))
	}
	if !st[0].Running || !st[0].Busy || st[0].State != "busy" {
		t.Fatalf("进程活着但健康检查不通时应报 busy：running=%v busy=%v state=%s",
			st[0].Running, st[0].Busy, st[0].State)
	}
	if st[0].Hint == "" {
		t.Fatal("busy 状态必须带提示，告诉用户别点启动")
	}

	// 没有托管进程、端口也不通 → 才是真正的「未启动」
	s2 := newBareSupervisor()
	s2.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint: dead, HealthPath: "/system_stats",
	}
	st2 := s2.Status()
	if st2[0].Running || st2[0].Busy || st2[0].State != "stopped" {
		t.Fatalf("没进程也不通时应报 stopped：running=%v busy=%v state=%s",
			st2[0].Running, st2[0].Busy, st2[0].State)
	}
}
func TestStatus_ReportsUnmanagedProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	s := newBareSupervisor()
	s.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint: srv.URL, HealthPath: "/system_stats",
		Exclusive: "comfyui",
	}
	st := s.Status()
	if len(st) != 1 {
		t.Fatalf("状态条目数应为 1，实际 %d", len(st))
	}
	// 端口通（外部实例）→ Running=true，但 Managed=false：这个区别是给人和 agent 看的
	if !st[0].Running {
		t.Fatal("端口可连通时 Running 应为 true")
	}
	if st[0].Managed {
		t.Fatal("不是本进程派生的实例，Managed 必须是 false")
	}
	raw, _ := json.Marshal(st[0])
	if len(raw) == 0 {
		t.Fatal("状态要能序列化给前端")
	}
}
