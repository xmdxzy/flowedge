package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 验证节点级模型覆盖真正传导到 LLM 请求体：
// - ChatWithModel(prompt, model) 使用显式 model
// - Chat(prompt) 回落到服务级默认模型
func TestChatWithModel_Propagation(t *testing.T) {
	var gotModel string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer srv.Close()

	s := NewLLMService(srv.URL, "default-model")

	// 节点级覆盖
	out, err := s.ChatWithModel("hi", "node-model")
	if err != nil || out != "pong" {
		t.Fatalf("ChatWithModel failed: out=%q err=%v", out, err)
	}
	if gotModel != "node-model" {
		t.Fatalf("node-level model not propagated: got %q want %q", gotModel, "node-model")
	}

	// 服务级默认回落
	if out, err := s.Chat("hi"); err != nil || out != "pong" {
		t.Fatalf("Chat failed: out=%q err=%v", out, err)
	}
	if gotModel != "default-model" {
		t.Fatalf("Chat fallback model wrong: got %q want %q", gotModel, "default-model")
	}

	// 空串覆盖等同回落
	if _, err := s.ChatWithModel("hi", ""); err != nil {
		t.Fatalf("ChatWithModel empty override failed: %v", err)
	}
	if gotModel != "default-model" {
		t.Fatalf("empty override should fall back, got %q", gotModel)
	}

	if calls != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", calls)
	}
}
