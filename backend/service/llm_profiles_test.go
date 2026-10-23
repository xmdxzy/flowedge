package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 保存期校验：名字重复 / 缺 model / base_url 不合法都要在保存时拦下，
// 不能等到运行期才 401 或 connection refused。
func TestLLMProfilesValidation(t *testing.T) {
	s := LoadLLMProfiles(t.TempDir() + "/llm-profiles.json")

	bad := [][]LLMProfile{
		{{Name: "", Model: "m", BaseURL: "http://x"}},
		{{Name: "a", Model: "", BaseURL: "http://x"}},
		{{Name: "a", Model: "m", BaseURL: ""}},
		{{Name: "a", Model: "m", BaseURL: "ftp://x"}},
		{{Name: "a", Model: "m", BaseURL: "http://x"}, {Name: "a", Model: "m", BaseURL: "http://y"}},
	}
	for i, list := range bad {
		if err := s.Replace(list, ""); err == nil {
			t.Errorf("第 %d 组非法输入应被拒", i)
		}
	}

	ok := []LLMProfile{{Name: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1/"}}
	if err := s.Replace(ok, "deepseek"); err != nil {
		t.Fatalf("合法输入被拒：%v", err)
	}
	got, exists := s.Get("deepseek")
	if !exists {
		t.Fatal("保存后取不到")
	}
	// 末尾斜杠要规范化掉，否则拼 /chat/completions 会出现双斜杠
	if strings.HasSuffix(got.BaseURL, "/") {
		t.Errorf("base_url 未规范化：%q", got.BaseURL)
	}
	if got.Backend != "openai" {
		t.Errorf("https 端点应判定为 openai 后端，实际 %q", got.Backend)
	}
}

// 鉴权走 Transport 注入：LLM 有 6 处请求构造点，逐处加会漏。
func TestLLMAuthTransportInjectsHeader(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	svc := NewLLMService(srv.URL, "test-model")
	authed := svc.WithAuth("sk-test")

	if _, err := authed.ChatWithModel("hi", ""); err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if seen != "Bearer sk-test" {
		t.Fatalf("鉴权头没注入，实际 %q", seen)
	}

	// 不带 key 的 service 不能凭空带 token（共享实例不允许被污染）
	seen = ""
	if _, err := svc.ChatWithModel("hi", ""); err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if seen != "" {
		t.Fatalf("原 service 不该带 Authorization，实际 %q", seen)
	}
}
