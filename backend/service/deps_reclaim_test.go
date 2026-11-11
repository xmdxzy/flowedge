package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回收的核心契约：**显存没涨就不能说「已释放」**。
// AGENTS 记过「假卸载」（接口回 ok、显存没还），这条测试就是防它再发生。
func TestVramReclaimResult_ReleasedOnlyWhenGained(t *testing.T) {
	// 涨了 3000MB → 算释放
	ok := VramReclaimResult{FreeBeforeMB: 800, FreeAfterMB: 3800, GainedMB: 3000}
	if !ok.IsReleased() {
		t.Error("显存涨了应算释放")
	}
	// 只涨 100MB（噪声）→ 不算
	noise := VramReclaimResult{FreeBeforeMB: 800, FreeAfterMB: 900, GainedMB: 100}
	if noise.IsReleased() {
		t.Error("100MB 属噪声，不能算释放")
	}
	// 显存反而变少（别的进程又吃走了）→ 更不能算释放
	shrunk := VramReclaimResult{FreeBeforeMB: 3000, FreeAfterMB: 1000, GainedMB: -2000}
	if shrunk.IsReleased() {
		t.Error("显存变少绝不能算释放")
	}
}

// comfyQueueIDs 必须能把 running 和 pending 都读出来——只读 pending 会漏掉
// 「卡在 VAE decode 的那条」，而那正是最需要清掉的。
func TestComfyQueueIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/queue" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"queue_running": [][]interface{}{{float64(1), "run-id-1"}},
			"queue_pending": [][]interface{}{{float64(2), "pend-id-2"}},
		})
	}))
	defer srv.Close()

	ids := comfyQueueIDs(srv.URL)
	if len(ids) != 2 {
		t.Fatalf("应读出 2 个 id，实际 %v", ids)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "run-id-1") || !strings.Contains(joined, "pend-id-2") {
		t.Errorf("running/pending 都要读到，实际 %v", ids)
	}
}

// 不可达时必须返回空而不是 panic（回收是尽力而为，不能反过来把引擎搞挂）
func TestComfyQueueIDs_Unreachable(t *testing.T) {
	if ids := comfyQueueIDs("http://127.0.0.1:1"); len(ids) != 0 {
		t.Errorf("不可达应返回空，实际 %v", ids)
	}
}

// deleteComfyQueue 要真的把 delete 列表发出去（不然清队列是空操作）
func TestDeleteComfyQueue_SendsPayload(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/queue" || r.Method != http.MethodPost {
			w.WriteHeader(400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := deleteComfyQueue(srv.URL, []string{"a", "b"})
	if n != 2 {
		t.Errorf("应报告删除 2 条，实际 %d", n)
	}
	list, _ := got["delete"].([]interface{})
	if len(list) != 2 {
		t.Errorf("delete 列表应含 2 项，实际 %v", got)
	}
	// 空列表不应发请求
	if n := deleteComfyQueue(srv.URL, nil); n != 0 {
		t.Errorf("空列表应返回 0，实际 %d", n)
	}
}

func TestIsComfySpec(t *testing.T) {
	for _, n := range []string{"comfyui", "comfy_h3", "comfy_moss"} {
		if !isComfySpec(n) {
			t.Errorf("%s 应识别为 comfy 系", n)
		}
	}
	for _, n := range []string{"tts", "asr", "ollama"} {
		if isComfySpec(n) {
			t.Errorf("%s 不应识别为 comfy 系", n)
		}
	}
}
