package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeComfyQueue 一个最小可用的 ComfyUI 队列替身：
// 只实现 /queue、/interrupt、/prompt、/history/<id>，够验证"中止是否真的生效"。
//
// 由来：2026-10-26 实测，/interrupt 打在一条**还在排队**的提交上什么都不会发生，
// 它 6 分钟后照样开始跑、又烧了 21 分钟。所以这一组测试要钉住两件事：
//  1. 中止能删掉 pending（不是只打 interrupt）
//  2. 取消信号能在下一次轮询就把生成链路打断，而不是等满 timeout
type fakeComfyQueue struct {
	mu         sync.Mutex
	pending    []string
	running    []string
	interrupts int
	cleared    int
	deleted    []string
	submitted  string
}

func (f *fakeComfyQueue) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/queue" && r.Method == http.MethodGet:
		writeQueue := func(ids []string) []interface{} {
			out := make([]interface{}, 0, len(ids))
			for i, id := range ids {
				out = append(out, []interface{}{i, id, map[string]interface{}{}, map[string]interface{}{}, []interface{}{}})
			}
			return out
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"queue_running": writeQueue(f.running),
			"queue_pending": writeQueue(f.pending),
		})
	case r.URL.Path == "/queue" && r.Method == http.MethodPost:
		var body struct {
			Clear  bool     `json:"clear"`
			Delete []string `json:"delete"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Clear {
			f.cleared += len(f.pending)
			f.pending = nil
		}
		for _, id := range body.Delete {
			f.deleted = append(f.deleted, id)
			kept := f.pending[:0]
			for _, p := range f.pending {
				if p != id {
					kept = append(kept, p)
				}
			}
			f.pending = kept
		}
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/interrupt":
		f.interrupts++
		f.running = nil // 模拟"停掉当前执行"
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/prompt":
		f.submitted = "p1"
		f.pending = append(f.pending, "p1")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"prompt_id": "p1"})
	case strings.HasPrefix(r.URL.Path, "/history/"):
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

// TestAbortPrompt_DeletesPending 排队中的提交：中止必须把它从 pending 删掉。
func TestAbortPrompt_DeletesPending(t *testing.T) {
	f := &fakeComfyQueue{pending: []string{"p1"}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	defer srv.Close()

	svc := &Text2ImageService{BaseURL: srv.URL}
	gone, wasRunning := svc.AbortPrompt(srv.URL, "p1")
	if !gone {
		t.Fatalf("AbortPrompt gone=false，pending 没删掉")
	}
	if wasRunning {
		t.Fatalf("wasRunning=true，但它本来在 pending")
	}
	if len(f.deleted) == 0 {
		t.Fatalf("没有调 /queue delete，只打了 interrupt —— 排队中的提交不会因此停下")
	}
}

// TestAbortPrompt_InterruptsRunning 正在跑的提交：要打 /interrupt 并确认离开队列。
func TestAbortPrompt_InterruptsRunning(t *testing.T) {
	f := &fakeComfyQueue{running: []string{"p2"}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	defer srv.Close()

	svc := &Text2ImageService{BaseURL: srv.URL}
	gone, wasRunning := svc.AbortPrompt(srv.URL, "p2")
	if !wasRunning {
		t.Fatalf("wasRunning=false，但它正在 running")
	}
	if !gone {
		t.Fatalf("interrupt 之后队列里还有它")
	}
	if f.interrupts == 0 {
		t.Fatalf("没有打 /interrupt")
	}
}

// TestRunGraphAtCtx_CancelStopsPrompt 取消信号必须打断生成链路，而不是等超时。
func TestRunGraphAtCtx_CancelStopsPrompt(t *testing.T) {
	f := &fakeComfyQueue{}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	defer srv.Close()

	svc := &Text2ImageService{BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	graph := map[string]interface{}{
		"1": map[string]interface{}{"class_type": "CheckpointLoaderSimple", "inputs": map[string]interface{}{}},
	}
	start := time.Now()
	_, err := svc.RunGraphAtCtx(ctx, srv.URL, graph, 60*time.Second, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("取消之后仍返回成功")
	}
	if !strings.Contains(err.Error(), "已取消") {
		t.Fatalf("错误信息没说明是取消：%v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("取消后 %s 才返回，等于没止损", elapsed)
	}
	if f.interrupts == 0 {
		t.Fatalf("取消时没有打 /interrupt")
	}
}

// TestRunGraphAtCtx_ReportsQueuePhase 排队 → 开始执行 必须能被上层看见，
// 因为看门狗要靠它把"排队时间"从"生成时间"里剔出去。
func TestRunGraphAtCtx_ReportsQueuePhase(t *testing.T) {
	f := &fakeComfyQueue{pending: []string{"p1"}}
	queueReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/queue" && r.Method == http.MethodGet {
			queueReads++
			if queueReads > 2 { // 前两次还在排队，之后开始执行
				f.mu.Lock()
				f.pending = nil
				f.running = []string{"p1"}
				f.mu.Unlock()
			}
		}
		f.handle(w, r)
	}))
	defer srv.Close()

	svc := &Text2ImageService{BaseURL: srv.URL}
	graph := map[string]interface{}{
		"1": map[string]interface{}{"class_type": "CheckpointLoaderSimple", "inputs": map[string]interface{}{}},
	}
	var mu sync.Mutex
	var phases []string
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Second)
		cancel()
	}()
	_, _ = svc.RunGraphAtCtx(ctx, srv.URL, graph, time.Minute, func(p string) {
		mu.Lock()
		phases = append(phases, p)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(phases) < 2 || phases[0] != "queued" {
		t.Fatalf("阶段序列 = %v，期望先 queued 后 running", phases)
	}
	sawRunning := false
	for _, p := range phases {
		if p == "running" {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatalf("阶段序列 = %v，没有观察到 running（看门狗就不会恢复计时）", phases)
	}
}

// TestRunGraphAtCtx_DroppedPromptFailsFast 提交被别人弄丢了要尽快失败，不能等满超时。
func TestRunGraphAtCtx_DroppedPromptFailsFast(t *testing.T) {
	f := &fakeComfyQueue{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prompt" {
			_, _ = w.Write([]byte(`{"prompt_id":"p1"}`)) // 收下但既不排队也不执行
			return
		}
		f.handle(w, r)
	}))
	defer srv.Close()

	svc := &Text2ImageService{BaseURL: srv.URL}
	graph := map[string]interface{}{
		"1": map[string]interface{}{"class_type": "CheckpointLoaderSimple", "inputs": map[string]interface{}{}},
	}
	start := time.Now()
	_, err := svc.RunGraphAtCtx(context.Background(), srv.URL, graph, 10*time.Minute, nil)
	if err == nil {
		t.Fatalf("提交丢了却等到成功/超时")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("改用 %s 才发现提交丢了，太慢", elapsed)
	}
}

