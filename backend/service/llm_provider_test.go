package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveLLMBackend 后端判定：显式优先，其次按地址特征
func TestResolveLLMBackend(t *testing.T) {
	cases := []struct{ base, explicit, want string }{
		{"http://localhost:11434", "", BackendOllama},
		{"http://127.0.0.1:11434", "", BackendOllama},
		{"http://127.0.0.1:8000", "", BackendOpenAI}, // vLLM 默认端口
		{"http://127.0.0.1:1234", "", BackendOpenAI}, // LM Studio 默认端口
		{"https://api.deepseek.com", "", BackendOpenAI},
		{"http://127.0.0.1:11434", "openai", BackendOpenAI},
		{"http://127.0.0.1:8000", "ollama", BackendOllama},
		{"http://127.0.0.1:8000", "vllm", BackendOpenAI},
		{"", "", BackendOllama}, // 无地址时保守当 ollama
	}
	for _, c := range cases {
		if got := ResolveLLMBackend(c.base, c.explicit); got != c.want {
			t.Errorf("ResolveLLMBackend(%q,%q) = %q, want %q", c.base, c.explicit, got, c.want)
		}
	}
}

// TestUnloadModel_OpenAIBackendIsExplicit 不支持卸载时必须显式报出来，
// 不能假装成功（否则换侧逻辑会以为显存已经还回来了）。
func TestUnloadModel_OpenAIBackendIsExplicit(t *testing.T) {
	svc := &LLMService{BaseURL: "http://127.0.0.1:8000", Model: "m", Backend: BackendOpenAI}
	err := svc.UnloadModel("")
	if err == nil {
		t.Fatal("expected explicit unsupported error, got nil")
	}
	if !IsUnloadUnsupported(err) {
		t.Errorf("error should be classified as unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("message should say it is unsupported: %s", err.Error())
	}
}

// TestVisionChat_OpenAIProtocolUsesImageURL openai 后端必须发 image_url 数组，
// 而不是 ollama 的 images 字段——否则 vLLM / LM Studio 直接 400。
func TestVisionChat_OpenAIProtocolUsesImageURL(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"一只猫"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	img := filepath.Join(dir, "a.png")
	if err := os.WriteFile(img, []byte("fakepng"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &LLMService{BaseURL: srv.URL, Model: "vlm", Backend: BackendOpenAI, client: srv.Client()}
	out, usage, err := svc.VisionChat("描述这张图", []string{img}, "")
	if err != nil {
		t.Fatalf("VisionChat: %v", err)
	}
	if out != "一只猫" || usage.CompletionTokens != 3 {
		t.Errorf("unexpected result %q %+v", out, usage)
	}
	msgs, _ := got["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", got["messages"])
	}
	content, _ := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected text + image parts, got %v", content)
	}
	second, _ := content[1].(map[string]interface{})
	if second["type"] != "image_url" {
		t.Errorf("second part type = %v, want image_url", second["type"])
	}
	urlObj, _ := second["image_url"].(map[string]interface{})
	if !strings.HasPrefix(urlObj["url"].(string), "data:image/png;base64,") {
		t.Errorf("bad data URI: %v", urlObj["url"])
	}
}

// TestVisionChat_OllamaProtocolUsesImagesArray 反向验证：ollama 后端仍用原生
// /api/chat 的 images 数组，改造不能把老路径改坏。
func TestVisionChat_OllamaProtocolUsesImagesArray(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"ok"},"prompt_eval_count":5,"eval_count":2}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	img := filepath.Join(dir, "a.png")
	if err := os.WriteFile(img, []byte("fakepng"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &LLMService{BaseURL: srv.URL, Model: "vlm", Backend: BackendOllama}
	out, _, err := svc.VisionChat("描述", []string{img}, "")
	if err != nil || out != "ok" {
		t.Fatalf("ollama vision path broken: %q %v", out, err)
	}
	msgs, _ := got["messages"].([]interface{})
	m0, _ := msgs[0].(map[string]interface{})
	imgs, _ := m0["images"].([]interface{})
	if len(imgs) != 1 {
		t.Errorf("ollama path must send images array, got %v", m0)
	}
}

// TestLLMErrorMentionsRealBackend 错误文案不能再写死 ollama——
// 用 vLLM 的用户看到 "ollama request failed" 会直接去查错方向。
func TestLLMErrorMentionsRealBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := &LLMService{BaseURL: srv.URL, Model: "m", Backend: BackendOpenAI, client: srv.Client()}
	_, _, err := svc.ChatWithModelUsage("hi", "")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "ollama") {
		t.Errorf("message must not blame ollama when backend is openai: %s", msg)
	}
	if !strings.Contains(msg, "OpenAI 兼容服务") {
		t.Errorf("message should name the real backend: %s", msg)
	}
}
