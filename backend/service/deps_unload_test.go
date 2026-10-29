package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeComfyForUnload 一个会报设备级空闲显存的假实例：收到 /free 后空闲涨上来。
func fakeComfyForUnload(t *testing.T, freeBefore, freeAfter int) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var free atomic.Int64
	var freeCalls, badBody atomic.Int32
	free.Store(int64(freeBefore))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system_stats":
			_, _ = w.Write([]byte(`{"devices":[{"vram_free":` + itoa(free.Load()<<20) + `}]}`))
		case "/free":
			freeCalls.Add(1)
			// 必须带两个 true：空 body 的 /free 不归还显存（这是老坑）
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12))
			var flags map[string]bool
			if json.Unmarshal(body, &flags) != nil || !flags["unload_models"] || !flags["free_memory"] {
				badBody.Add(1)
			}
			free.Store(int64(freeAfter))
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &freeCalls, &badBody
}

// comfy 实例：卸载后要报出「还回来多少显存」，且 /free 必须带两个 true。
func TestUnload_ComfyReportsFreed(t *testing.T) {
	srv, freeCalls, badBody := fakeComfyForUnload(t, 4000, 20000)

	s := newBareSupervisor()
	s.specs["comfyui"] = &DepSpec{Name: "comfyui", Label: "ComfyUI", Endpoint: srv.URL, HealthPath: "/system_stats"}

	res, err := s.Unload("comfyui")
	if err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if !res.Supported {
		t.Fatal("ComfyUI 应支持卸载")
	}
	if n := freeCalls.Load(); n != 1 {
		t.Fatalf("/free 应被调用 1 次，实际 %d", n)
	}
	if n := badBody.Load(); n != 0 {
		t.Fatal("/free 的请求体必须带 unload_models 与 free_memory 两个 true")
	}
	// 4000 → 20000，应报出约 16000MB 的回收
	if res.FreedMB < 15000 {
		t.Fatalf("应报出约 16000MB 回收，实际 %d（free=%d）", res.FreedMB, res.FreeMB)
	}
	if !strings.Contains(res.Note, "进程保留") {
		t.Fatalf("说明里要讲清进程还在，否则用户会以为服务停了：%s", res.Note)
	}
}

// Ollama：逐个 keep_alive=0，并数清楚卸了几个。
func TestUnload_OllamaUnloadsEachModel(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5:14b"},{"name":"dsh-lens-8b"}]}`))
		case "/api/generate":
			posts.Add(1)
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12))
			if !strings.Contains(string(body), `"keep_alive":0`) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	s := newBareSupervisor()
	s.specs["ollama"] = &DepSpec{Name: "ollama", Label: "Ollama", Endpoint: srv.URL, HealthPath: "/api/ps"}

	res, err := s.Unload("ollama")
	if err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if n := posts.Load(); n != 2 {
		t.Fatalf("两个驻留模型应各发一次 generate，实际 %d", n)
	}
	if !strings.Contains(res.Note, "2 个") {
		t.Fatalf("说明里应有卸载个数：%s", res.Note)
	}
}

// 没有卸载接口的依赖要如实说「不支持」，不能假装成功（§0.6）。
func TestUnload_UnsupportedIsHonest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newBareSupervisor()
	s.specs["asr"] = &DepSpec{Name: "asr", Label: "ASR", Endpoint: srv.URL, HealthPath: "/health"}

	res, err := s.Unload("asr")
	if err != nil {
		t.Fatalf("不支持的依赖不该返回错误（它不是失败）：%v", err)
	}
	if res.Supported {
		t.Fatal("ASR 没有卸载接口，Supported 必须是 false")
	}
	if !strings.Contains(res.Note, "没有卸载接口") {
		t.Fatalf("要说清为什么不支持：%s", res.Note)
	}
}

// 没运行的依赖：直接说「无需卸载」，不要去打一个不存在的端口。
func TestUnload_NotRunning(t *testing.T) {
	s := newBareSupervisor()
	s.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint: "http://127.0.0.1:1", HealthPath: "/system_stats",
	}
	if _, err := s.Unload("comfy_h3"); err == nil || !strings.Contains(err.Error(), "未运行") {
		t.Fatalf("未运行时应报「未运行」，实际：%v", err)
	}
}

func TestUnload_UnknownDep(t *testing.T) {
	s := newBareSupervisor()
	if _, err := s.Unload("nope"); err == nil || !strings.Contains(err.Error(), "未知依赖") {
		t.Fatalf("未知依赖应报错，实际：%v", err)
	}
}

// 「端口通但不是我们起的」——那晚 27 倍慢的真凶必须能被查询到。
func TestServingButUnmanaged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	s := newBareSupervisor()
	s.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint: srv.URL, HealthPath: "/system_stats",
	}

	// 端口通、procs 里没有 → 外部实例
	if !s.ServingButUnmanaged("comfy_h3") {
		t.Fatal("端口通但没托管进程时，应判为「外部实例」")
	}

	// 我们自己派生的进程活着 → 不是外部
	s.mu.Lock()
	s.procs["comfy_h3"] = &exec.Cmd{}
	s.mu.Unlock()
	if s.ServingButUnmanaged("comfy_h3") {
		t.Fatal("有本进程派生的进程时，不该判成外部实例")
	}

	// 未运行的依赖、未知依赖、nil supervisor 都不该误报
	s2 := newBareSupervisor()
	s2.specs["comfy_h3"] = &DepSpec{Name: "comfy_h3", Endpoint: "http://127.0.0.1:1", HealthPath: "/system_stats"}
	if s2.ServingButUnmanaged("comfy_h3") {
		t.Fatal("端口不通时不该判成外部实例")
	}
	if s2.ServingButUnmanaged("nope") {
		t.Fatal("未知依赖不该判成外部实例")
	}
	var nilSup *Supervisor
	if nilSup.ServingButUnmanaged("comfy_h3") {
		t.Fatal("nil supervisor 不该判成外部实例")
	}
}
