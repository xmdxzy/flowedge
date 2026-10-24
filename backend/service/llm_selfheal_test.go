package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllama 只认 good:1b；其他模型返回 Ollama 风格的 404。
func fakeOllama(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"good:1b"},{"name":"bge-m3:latest"}]}`))
		case "/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			*calls = append(*calls, req.Model)
			if req.Model != "good:1b" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"model '` + req.Model + `' not found","type":"not_found_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"好了"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// 引擎先起、Ollama 后起时，启动阶段探不到模型会退到兜底名（本机不存在）。
// 等 Ollama 起来后，第一次调用要能自己重新挑一个真存在的模型再试——
// 否则用户看到的是"Ollama 404"，会去怀疑端口/代理。
func TestLLMSelfHealsDefaultModel(t *testing.T) {
	var calls []string
	srv := fakeOllama(t, &calls)
	defer srv.Close()

	svc := NewLLMService(srv.URL, "qwen2.5-coder:7b") // 兜底名，本机没有
	out, err := svc.ChatWithTemperature("说个词", "", 0)
	if err != nil {
		t.Fatalf("应当自愈后成功，实际报错：%v", err)
	}
	if out != "好了" {
		t.Errorf("输出 = %q", out)
	}
	if svc.Model != "good:1b" {
		t.Errorf("默认模型应更新为 good:1b，实际 %q", svc.Model)
	}
	if len(calls) != 2 || calls[0] != "qwen2.5-coder:7b" || calls[1] != "good:1b" {
		t.Errorf("调用序列应为 [兜底名, good:1b]，实际 %v", calls)
	}
}

// 调用方点名要某个模型时，绝不能被悄悄替换（换模型=换创作质量）。
func TestLLMDoesNotReplaceExplicitModel(t *testing.T) {
	var calls []string
	srv := fakeOllama(t, &calls)
	defer srv.Close()

	svc := NewLLMService(srv.URL, "good:1b")
	_, err := svc.ChatWithTemperature("说个词", "not-there:7b", 0)
	if err == nil {
		t.Fatal("点名要的模型不存在时必须报错，而不是换一个")
	}
	if !strings.Contains(err.Error(), "not-there:7b") {
		t.Errorf("错误要点名缺哪个模型：%v", err)
	}
	if svc.Model != "good:1b" {
		t.Errorf("默认模型不该被改写，实际 %q", svc.Model)
	}
	if len(calls) != 1 {
		t.Errorf("显式模型失败后不该重试，实际调用 %v", calls)
	}
}

// 自愈只做一次：再失败就如实报错，不要无限循环。
func TestLLMSelfHealOnce(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write([]byte(`{"models":[{"name":"still-bad:1b"}]}`))
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Model)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model '` + req.Model + `' not found"}}`))
	}))
	defer srv.Close()

	svc := NewLLMService(srv.URL, "first-bad:1b")
	if _, err := svc.ChatWithTemperature("x", "", 0); err == nil {
		t.Fatal("一直不存在时必须报错")
	}
	if len(calls) > 2 {
		t.Fatalf("最多重试一次，实际调用了 %d 次：%v", len(calls), calls)
	}
}
